// Package setup installs and removes git-sync on a machine.
package setup

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
)

type Options struct {
	BaseDir string
	Peers   []config.Peer // every other machine in the mesh
	Repos   []string      // the allowlist chosen by the picker (or --all / --repos)
	Self    string        // path to the binary to install; defaults to os.Executable()
	Out     io.Writer

	// Peer provisioning (Task 11).
	NoPeer   bool
	SelfHost string
	SelfUser string
	// PeerBaseDir overrides the derived base_dir for a peer that did not
	// specify its own (config.Peer.BaseDir wins when a peer sets it).
	PeerBaseDir string
}

// HookNames are the git hooks git-sync installs. post-commit broadcasts;
// the other two refuse to run while this machine is receiving that repo.
var HookNames = []string{"post-commit", "pre-commit", "pre-push"}

// hookShim is what git actually executes on every commit. It is a shell stub
// rather than the binary itself so that re-installing a new binary cannot
// race a commit that is executing the old one.
const hookShimTemplate = `#!/bin/sh
# Installed by git-sync. Removed by 'git-sync uninstall'.
exec %q hook %s
`

func hookShim(binPath, hookName string) string {
	return fmt.Sprintf(hookShimTemplate, binPath, hookName)
}

func Install(o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}

	base, err := filepath.Abs(o.BaseDir)
	if err != nil {
		return fmt.Errorf("resolving base_dir: %w", err)
	}
	// The hook runs from arbitrary working directories, so a relative
	// base_dir would be meaningless once stored.
	fi, err := os.Stat(base)
	if err != nil {
		return fmt.Errorf("base_dir %s: %w", base, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("base_dir %s is not a directory", base)
	}

	self := o.Self
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return fmt.Errorf("locating this binary: %w", err)
		}
	}

	for _, d := range []string{config.Home(), config.HooksDir(), config.LocksDir(), filepath.Dir(config.BinPath())} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// Copy the binary in, so the install keeps working if the source tree
	// moves or the repo is deleted.
	if err := copyExecutable(self, config.BinPath()); err != nil {
		return fmt.Errorf("installing the binary: %w", err)
	}

	// The hook shims are what git actually execs, so each gets the same
	// temp-file-then-rename treatment as the binary: a concurrent commit
	// must never see a half-written file.
	for _, name := range HookNames {
		shim := hookShim(config.BinPath(), name)
		hookPath := filepath.Join(config.HooksDir(), name)
		if err := writeFileAtomic(hookPath, []byte(shim), 0o755); err != nil {
			return fmt.Errorf("writing the %s hook: %w", name, err)
		}
	}

	fmt.Fprintf(o.Out, "installed into %s\n", config.Home())

	cfg := config.Config{
		BaseDir: base, Peers: o.Peers, Repos: o.Repos,
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	fmt.Fprintf(o.Out, "syncing %d repos under %s with %d peer(s)\n",
		len(cfg.Repos), base, len(cfg.PeerList()))

	// core.hooksPath is global and exclusive: this points git at our hooks
	// for every hook type in every repo on the machine, replacing any
	// repo-local hooks. See Known Limitations in the README.
	if out, err := exec.Command("git", "config", "--global", "core.hooksPath", config.HooksDir()).CombinedOutput(); err != nil {
		return fmt.Errorf("setting core.hooksPath: %w: %s", err, out)
	}
	fmt.Fprintf(o.Out, "set global core.hooksPath to %s\n", config.HooksDir())

	if o.NoPeer {
		fmt.Fprintln(o.Out, "skipping peer provisioning (--no-peer)")
		fmt.Fprintln(o.Out, "done.")
		return nil
	}

	selfHost := o.SelfHost
	if selfHost == "" {
		selfHost, _ = os.Hostname()
	}
	selfUser := o.SelfUser
	if selfUser == "" {
		selfUser = os.Getenv("USER")
	}

	// Provision every machine in the mesh. One peer being unreachable is a
	// warning, not a reason to abandon the others - each gets its own
	// independent attempt and its own report. A real provisioning failure
	// (e.g. an architecture mismatch) is the same: it must not stop later
	// peers from being provisioned, and it must not skip the initial-sync
	// stage for the peers that did succeed - this machine's own config
	// already lists every peer, so it will keep trying to notify an
	// unprovisioned one forever if we bail out here.
	var anyUnreachable bool
	var failed []string
	for _, p := range cfg.PeerList() {
		override := p.BaseDir
		if override == "" {
			override = o.PeerBaseDir
		}
		err := ProvisionPeer(PeerOptions{
			Cfg: cfg, Peer: p,
			Self: config.BinPath(), SelfHost: selfHost, SelfUser: selfUser,
			PeerBaseDir: override, Out: o.Out,
		})
		switch {
		case err == nil:
			// ProvisionPeer already reported what it did.
		case IsPeerUnreachable(err):
			// The local half is correct and useful on its own.
			fmt.Fprintf(o.Out, "WARNING: peer %s not provisioned: unreachable.\n", p.Host)
			anyUnreachable = true
		default:
			fmt.Fprintf(o.Out, "WARNING: peer %s not provisioned: %v\n", p.Host, err)
			failed = append(failed, p.Host)
		}
	}
	switch {
	case len(failed) > 0:
		fmt.Fprintln(o.Out, "This machine is set up. Fix the errors above and re-run install to finish provisioning.")
		return fmt.Errorf("failed to provision %d of %d peer(s): %s",
			len(failed), len(cfg.PeerList()), strings.Join(failed, ", "))
	case anyUnreachable:
		fmt.Fprintln(o.Out, "This machine is set up. Re-run install once every peer is up.")
	default:
		fmt.Fprintln(o.Out, "done. Every machine is set up.")
	}
	return nil
}

// Uninstall removes the hook, the binary, and config.AskpassPath (left over
// from the password era on an upgrade from an older install). It keeps
// config.toml and activity.jsonl so `git-sync report` still works on your
// history, unless purge is set.
func Uninstall(purge bool, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	// Only unset core.hooksPath if it is still ours. Another tool may own it
	// now, and clobbering that would break their setup.
	current, _ := exec.Command("git", "config", "--global", "core.hooksPath").Output()
	if strings.TrimSpace(string(current)) == config.HooksDir() {
		if err := exec.Command("git", "config", "--global", "--unset", "core.hooksPath").Run(); err != nil {
			return fmt.Errorf("unsetting core.hooksPath: %w", err)
		}
		fmt.Fprintln(out, "unset global core.hooksPath")
	}

	for _, p := range []string{
		config.HooksDir(), filepath.Dir(config.BinPath()), config.LocksDir(),
		config.AskpassPath(), // left over from the password era; removed on upgrade
	} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "removed hooks and binary")

	if purge {
		if err := os.RemoveAll(config.Home()); err != nil {
			return err
		}
		fmt.Fprintf(out, "purged %s\n", config.Home())
		return nil
	}
	fmt.Fprintf(out, "kept your config and activity history in %s\n", config.Home())
	fmt.Fprintln(out, "(git-sync uninstall --purge removes those too)")
	return nil
}

// UninstallMesh removes git-sync from every machine in the mesh, this one
// last: a peer is reachable only while our own config still names it, so
// uninstalling here first would strand any peer we could not reach.
func UninstallMesh(cfg config.Config, purge bool, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	var unreachable []config.Peer
	for _, p := range cfg.PeerList() {
		cmd := "~/.gitsync/bin/git-sync uninstall --local"
		if purge {
			cmd += " --purge"
		}
		if err := ssh(p.Target(), cmd); err != nil {
			unreachable = append(unreachable, p)
			continue
		}
		fmt.Fprintf(out, "uninstalled git-sync on %s\n", p.Host)
	}

	if err := Uninstall(purge, out); err != nil {
		return err
	}

	for _, p := range unreachable {
		fmt.Fprintf(out, "WARNING: could not reach %s; run there by hand: git-sync uninstall\n", p.Host)
	}
	return nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// Write to a temp file and rename, so a concurrent hook never executes a
	// half-written binary.
	tmp := dst + ".tmp"
	outF, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(outF, in); err != nil {
		outF.Close()
		return err
	}
	if err := outF.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// writeFileAtomic writes data to a temp file next to dst and renames it into
// place, so a concurrent hook invocation or ssh call never sees a
// half-written file - the same guarantee copyExecutable gives the binary.
func writeFileAtomic(dst string, data []byte, perm os.FileMode) error {
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
