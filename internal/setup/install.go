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
	BaseDir  string
	PeerHost string
	PeerUser string
	Repos    []string // the allowlist chosen by the picker (or --all / --repos)
	Self     string   // path to the binary to install; defaults to os.Executable()
	Out      io.Writer

	// Peer provisioning (Task 11).
	NoPeer      bool
	SelfHost    string
	SelfUser    string
	PeerBaseDir string
}

// hookShim is what git actually executes on every commit. It is a shell stub
// rather than the binary itself so that re-installing a new binary cannot
// race a commit that is executing the old one.
const hookShimTemplate = `#!/bin/sh
# Installed by git-sync. Removed by 'git-sync uninstall'.
exec %q hook post-commit
`

// askpassShimTemplate is what ssh execs when it needs the peer's password.
// SSH_ASKPASS names one executable and passes it the prompt text, so the
// account has to be baked in here rather than passed at call time.
const askpassShimTemplate = `#!/bin/sh
# Installed by git-sync. Prints the peer's ssh password from the keychain.
exec %q askpass %q "$@"
`

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

	// The hook shim and askpass shim are what git and ssh actually exec, so
	// they get the same temp-file-then-rename treatment as the binary: a
	// concurrent commit or ssh invocation must never see a half-written file.
	shim := fmt.Sprintf(hookShimTemplate, config.BinPath())
	hookPath := filepath.Join(config.HooksDir(), "post-commit")
	if err := writeFileAtomic(hookPath, []byte(shim), 0o755); err != nil {
		return fmt.Errorf("writing the hook: %w", err)
	}

	// The askpass shim (Task 12). Written unconditionally and harmless when
	// key auth is in use: nothing sets SSH_ASKPASS unless a password is
	// actually stored. 0700 - it reads a credential out of the keychain.
	askpass := fmt.Sprintf(askpassShimTemplate, config.BinPath(), o.PeerUser+"@"+o.PeerHost)
	if err := writeFileAtomic(config.AskpassPath(), []byte(askpass), 0o700); err != nil {
		return fmt.Errorf("writing the askpass helper: %w", err)
	}
	fmt.Fprintf(o.Out, "installed into %s\n", config.Home())

	cfg := config.Config{
		BaseDir: base, PeerHost: o.PeerHost, PeerUser: o.PeerUser, Repos: o.Repos,
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	fmt.Fprintf(o.Out, "syncing %d repos under %s with %s@%s\n",
		len(cfg.Repos), base, cfg.PeerUser, cfg.PeerHost)

	// core.hooksPath is global and exclusive: this points git at our hooks
	// for every hook type in every repo on the machine, replacing any
	// repo-local hooks. See Known Limitations in the README.
	if out, err := exec.Command("git", "config", "--global", "core.hooksPath", config.HooksDir()).CombinedOutput(); err != nil {
		return fmt.Errorf("setting core.hooksPath: %w: %s", err, out)
	}
	fmt.Fprintf(o.Out, "set global core.hooksPath to %s\n", config.HooksDir())

	// Peer provisioning is appended here in Task 11, replacing what used to
	// be a bare reachability check.
	return nil
}

// Uninstall removes the hook, the binary, and the askpass shim. It keeps
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

	for _, p := range []string{config.HooksDir(), filepath.Dir(config.BinPath()), config.LocksDir(), config.AskpassPath()} {
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
