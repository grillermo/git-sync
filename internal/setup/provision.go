package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/sshx"
)

// PeerOptions describes the peer half of an install.
type PeerOptions struct {
	Cfg         config.Config // this machine's config; Repos is copied verbatim
	Peer        config.Peer   // the machine being provisioned
	Self        string        // path to the binary to send
	SelfHost    string        // this machine's hostname, as the peer sees it
	SelfUser    string        // the account the peer should ssh back into
	PeerBaseDir string        // overrides the derived peer base_dir
	Out         io.Writer
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
	target := o.Peer.Target()

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

	// 5. The peer's config: the same repos, its own base_dir, and the rest of
	//    the mesh - every machine except itself, plus us.
	peerCfg := o.Cfg.WithoutPeer(o.Peer.Host)
	peerCfg.BaseDir = o.peerBase(peerHome)
	peerCfg.Repos = o.Cfg.Repos
	peerCfg.Peers = append(peerCfg.Peers, config.Peer{
		Host: o.SelfHost, User: o.SelfUser, BaseDir: o.Cfg.BaseDir,
	})
	// MarshalFor, not Marshal: Marshal would self-filter using THIS
	// machine's hostname, not the peer's, and could drop or keep the wrong
	// entries in the file we are about to stream to it.
	toml, err := peerCfg.MarshalFor(o.Peer.Host)
	if err != nil {
		return err
	}
	cfgCmd := fmt.Sprintf(
		"cat > %s/config.toml.tmp && mv %s/config.toml.tmp %s/config.toml",
		peerGitsync, peerGitsync, peerGitsync)
	if err := sshIn(target, cfgCmd, bytes.NewReader(toml)); err != nil {
		return err
	}

	// 6. The hook shims, written then renamed for the same reason as the
	//    binary: a commit landing on the peer mid-transfer must never exec a
	//    half-written shim.
	for _, name := range HookNames {
		shim := hookShim(peerGitsync+"/bin/git-sync", name)
		hookCmd := fmt.Sprintf(
			"cat > %s/hooks/%s.tmp && chmod +x %s/hooks/%s.tmp && mv %s/hooks/%s.tmp %s/hooks/%s",
			peerGitsync, name, peerGitsync, name, peerGitsync, name, peerGitsync, name)
		if err := sshIn(target, hookCmd, strings.NewReader(shim)); err != nil {
			return err
		}
	}

	// 7. Point the peer's git at it.
	if err := ssh(target, "git config --global core.hooksPath "+peerGitsync+"/hooks"); err != nil {
		return err
	}

	fmt.Fprintf(o.Out, "provisioned %s: %d repos, base_dir %s\n",
		o.Peer.Host, len(o.Cfg.Repos), peerCfg.BaseDir)
	fmt.Fprintf(o.Out, "  the peer will reach back at %s@%s\n", o.SelfUser, o.SelfHost)

	return nil
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

// Probe asks the peer the two questions every later step depends on. A
// failure here is what "unreachable" means: nothing has been written yet.
func Probe(target string) (PeerProbe, error) {
	var p PeerProbe
	uname, err := sshOut(target, "uname -sm")
	if err != nil {
		return p, fmt.Errorf("%w: %s: %v", errPeerUnreachable, target, err)
	}
	home, err := sshOut(target, "echo $HOME")
	if err != nil {
		return p, fmt.Errorf("%w: %s: %v", errPeerUnreachable, target, err)
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
