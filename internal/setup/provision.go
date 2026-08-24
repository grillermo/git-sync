package setup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/secret"
	"github.com/grillermo/git-sync/internal/sshx"
)

// PeerOptions describes the peer half of an install.
type PeerOptions struct {
	Cfg         config.Config // this machine's config; Repos is copied verbatim
	Self        string        // path to the binary to send
	SelfHost    string        // this machine's hostname, as the peer sees it
	SelfUser    string        // the account the peer should ssh back into
	PeerBaseDir string        // overrides the derived peer base_dir
	Out         io.Writer
	// In, when set to a terminal, lets ProvisionPeer offer to set up the way
	// back: a password on the peer for reaching this machine. Left nil in
	// tests (and by any non-interactive caller), in which case this step is
	// skipped with a message rather than a hang.
	In io.Reader
}

var errPeerUnreachable = errors.New("peer unreachable")

// IsPeerUnreachable lets Install treat an offline peer as a warning: the local
// half of the install is still correct, and re-running later finishes the job.
func IsPeerUnreachable(err error) bool { return errors.Is(err, errPeerUnreachable) }

// ProvisionPeer installs git-sync on the other machine over ssh, so that
// nothing has to be typed there. Idempotent, like the local install.
func ProvisionPeer(o PeerOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	target := Target(o.Cfg)

	// 1-2. The binary is copied verbatim, so a mismatched peer could never run
	//      it; and every path we write must be absolute, which needs the peer's
	//      home. Both are answered before anything is written.
	probe, err := Probe(target)
	if err != nil {
		return err
	}
	if err := checkSamePlatform(probe.Uname); err != nil {
		return err
	}
	peerHome := probe.Home
	peerGitsync := peerHome + "/.gitsync"

	// 3. Directories.
	if err := ssh(target, fmt.Sprintf("mkdir -p %s/bin %s/hooks %s/locks",
		peerGitsync, peerGitsync, peerGitsync)); err != nil {
		return err
	}

	// 4. The binary, written then renamed so a commit running on the peer
	//    never executes a half-copied file.
	bin, err := os.Open(o.Self)
	if err != nil {
		return err
	}
	defer bin.Close()
	install := fmt.Sprintf(
		"cat > %s/bin/git-sync.tmp && chmod +x %s/bin/git-sync.tmp && mv %s/bin/git-sync.tmp %s/bin/git-sync",
		peerGitsync, peerGitsync, peerGitsync, peerGitsync)
	if err := sshIn(target, install, bin); err != nil {
		return fmt.Errorf("copying the binary to %s: %w", target, err)
	}

	// 5. The mirrored config: same repos, the peer's own base_dir, pointing
	//    back at us.
	peerCfg := config.Config{
		BaseDir:  o.peerBase(peerHome),
		PeerHost: o.SelfHost,
		PeerUser: o.SelfUser,
		Repos:    o.Cfg.Repos,
	}
	toml, err := peerCfg.Marshal()
	if err != nil {
		return err
	}
	cfgCmd := fmt.Sprintf(
		"cat > %s/config.toml.tmp && mv %s/config.toml.tmp %s/config.toml",
		peerGitsync, peerGitsync, peerGitsync)
	if err := sshIn(target, cfgCmd, bytes.NewReader(toml)); err != nil {
		return err
	}

	// 6. The hook shim, written then renamed for the same reason as the binary:
	//    a commit landing on the peer mid-transfer must never exec a half-written
	//    shim.
	shim := fmt.Sprintf(hookShimTemplate, peerGitsync+"/bin/git-sync")
	hookCmd := fmt.Sprintf(
		"cat > %s/hooks/post-commit.tmp && chmod +x %s/hooks/post-commit.tmp && mv %s/hooks/post-commit.tmp %s/hooks/post-commit",
		peerGitsync, peerGitsync, peerGitsync, peerGitsync)
	if err := sshIn(target, hookCmd, strings.NewReader(shim)); err != nil {
		return err
	}

	// 7. Point the peer's git at it.
	if err := ssh(target, "git config --global core.hooksPath "+peerGitsync+"/hooks"); err != nil {
		return err
	}

	fmt.Fprintf(o.Out, "provisioned %s: %d repos, base_dir %s\n",
		o.Cfg.PeerHost, len(o.Cfg.Repos), peerCfg.BaseDir)
	fmt.Fprintf(o.Out, "  the peer will reach back at %s@%s\n", o.SelfUser, o.SelfHost)

	o.setUpTheWayBack(target, peerGitsync)
	return nil
}

// setUpTheWayBack gives the peer a password for reaching this machine, so
// syncing works in both directions. Runtime is symmetric: the peer runs its
// own push, which ssh's back to us, and if this machine also wants a
// password, the peer needs one stored too. Best-effort and silent about
// failures beyond a message - the outbound direction (already provisioned
// above) still works either way.
func (o PeerOptions) setUpTheWayBack(target, peerGitsync string) {
	selfAccount := o.SelfUser + "@" + o.SelfHost

	if o.In == nil {
		fmt.Fprintf(o.Out, "  if %s needs a password to reach %s, run "+
			"'git-sync install' there too (or copy your ssh key with ssh-copy-id)\n",
			o.Cfg.PeerHost, selfAccount)
		return
	}

	// The usual case is the same account and the same password on both
	// machines: if we ourselves needed a password to reach the peer, that is
	// the one already sitting, verified, in our own keychain right now.
	pw, err := secret.Get(target)
	if err != nil {
		// We reach the peer via key auth; nothing to offer as a default, and
		// nothing was ever typed to reuse.
		return
	}

	if !confirmYesDefault(o.Out, o.In,
		fmt.Sprintf("use the same password for the way back (%s)? [Y/n]: ", selfAccount)) {
		return
	}

	// 2. Store it on the peer, over the encrypted channel - never in the
	// remote command line, never in the peer's shell history.
	savepass := fmt.Sprintf("%s/bin/git-sync savepass '%s'", peerGitsync, selfAccount)
	if err := sshIn(target, savepass, bytes.NewReader(pw)); err != nil {
		fmt.Fprintf(o.Out, "  could not save a password on %s: %v\n", o.Cfg.PeerHost, err)
		return
	}

	// 3. The peer's own askpass shim, naming the account it will reach us at.
	//    Same temp-file-then-rename treatment as the hook and binary.
	shim := fmt.Sprintf(askpassShimTemplate, peerGitsync+"/bin/git-sync", selfAccount)
	shimCmd := fmt.Sprintf(
		"cat > %s/askpass.tmp && chmod 700 %s/askpass.tmp && mv %s/askpass.tmp %s/askpass",
		peerGitsync, peerGitsync, peerGitsync, peerGitsync)
	if err := sshIn(target, shimCmd, strings.NewReader(shim)); err != nil {
		fmt.Fprintf(o.Out, "  could not write the peer's askpass helper: %v\n", err)
		return
	}
	fmt.Fprintf(o.Out, "  saved a password on %s for reaching %s\n", o.Cfg.PeerHost, selfAccount)
}

// confirmYesDefault asks a yes/no question defaulting to yes: only an
// explicit n/no answers false, EOF included, since a script feeding no input
// at all should not be read as consent.
func confirmYesDefault(w io.Writer, r io.Reader, question string) bool {
	fmt.Fprint(w, question)
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "n", "no":
		return false
	}
	return true
}

func (o PeerOptions) peerBase(peerHome string) string {
	return PeerBase(o.Cfg.BaseDir, peerHome, o.PeerBaseDir)
}

// PeerBase derives the peer's sync root: the same path relative to $HOME as
// ours, unless overridden. Exported because the counterpart check needs the
// exact same answer as provisioning, before provisioning has run.
func PeerBase(ourBase, peerHome, override string) string {
	if override != "" {
		return override
	}
	if rel, err := filepath.Rel(os.Getenv("HOME"), ourBase); err == nil &&
		!strings.HasPrefix(rel, "..") {
		return peerHome + "/" + filepath.ToSlash(rel)
	}
	// os.Getenv("HOME") does not prefix ourBase - this happens in tests that
	// exercise the derivation with a synthetic path, and could happen for real
	// if this process's HOME is not the account's actual home. Fall back to
	// the conventional Unix layout (/Users/<name>/... or /home/<name>/...)
	// and strip the same two leading segments, rather than assuming the two
	// machines share an absolute path that has no reason to line up.
	if rel, ok := stripConventionalHomePrefix(ourBase); ok {
		return peerHome + "/" + rel
	}
	// Not even that pattern matches; fall back to the same absolute path.
	return ourBase
}

// stripConventionalHomePrefix strips a leading /Users/<name> or /home/<name>
// from p, the two conventional forms of a Unix home directory, and reports
// whether it found one.
func stripConventionalHomePrefix(p string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(filepath.ToSlash(filepath.Clean(p)), "/"), "/")
	if len(parts) < 3 || (parts[0] != "Users" && parts[0] != "home") {
		return "", false
	}
	return strings.Join(parts[2:], "/"), true
}

// PeerProbe is what the peer says about itself. Gathered once and reused, so
// install does not ask the same two questions over three ssh connections.
type PeerProbe struct {
	Uname string // "Darwin arm64"
	Home  string // absolute, trimmed
}

// Probe asks the peer the two questions every later step depends on. A failure
// here is what "unreachable" means: nothing has been written yet - unless the
// peer is reachable but wants a password, which classify tells apart from a
// real connectivity failure.
func Probe(target string) (PeerProbe, error) {
	var p PeerProbe
	uname, err := sshOut(target, "uname -sm")
	if err != nil {
		return p, classify(target, sshErrText(err), err)
	}
	home, err := sshOut(target, "echo $HOME")
	if err != nil {
		return p, classify(target, sshErrText(err), err)
	}
	p.Uname = strings.TrimSpace(uname)
	p.Home = strings.TrimSpace(home)
	if p.Home == "" {
		// A reachable ssh that answers with nothing useful is, for our
		// purposes, no different from being unreachable: nothing has been
		// written, and the fix is the same - re-run once the peer is set up.
		return p, fmt.Errorf("%w: %s: could not determine the peer's home directory", errPeerUnreachable, target)
	}
	return p, nil
}

// Target is the ssh destination for a config, in one place so the check and
// the provisioning cannot disagree about it.
func Target(c config.Config) string { return c.PeerUser + "@" + c.PeerHost }

func checkSamePlatform(uname string) error {
	fields := strings.Fields(strings.TrimSpace(uname))
	if len(fields) < 2 {
		return fmt.Errorf("could not read the peer's platform from %q", uname)
	}
	wantOS, wantArch := unameOS(), unameArch()
	if !strings.EqualFold(fields[0], wantOS) || fields[1] != wantArch {
		return fmt.Errorf(
			"peer is %s %s but this machine is %s %s; the binary is copied verbatim, "+
				"so install git-sync on the peer by hand with a matching build",
			fields[0], fields[1], wantOS, wantArch)
	}
	return nil
}

func unameOS() string {
	if runtime.GOOS == "darwin" {
		return "Darwin"
	}
	return "Linux"
}

func unameArch() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return runtime.GOARCH // arm64 prints as arm64 on both platforms
}

func ssh(target, remote string) error {
	out, err := sshx.Command(target, remote).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh %s %q: %w: %s", target, remote, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sshOut(target, remote string) (string, error) {
	out, err := sshx.Command(target, remote).Output()
	return string(out), err
}

func sshIn(target, remote string, stdin io.Reader) error {
	cmd := sshx.Command(target, remote)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh %s %q: %w: %s", target, remote, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sshErrText extracts whatever text is available from an ssh failure - the
// captured stderr when Output() produced an *exec.ExitError, or the error's
// own message otherwise - so classify has something to look for
// "Permission denied" in.
func sshErrText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(ee.Stderr)
	}
	return err.Error()
}
