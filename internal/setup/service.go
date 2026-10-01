package setup

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
)

// The login service runs `git-sync watch` for the length of the user's
// session: on macOS a LaunchAgent, on Linux a systemd user unit. watch
// announces this machine to the mesh when it starts and again every time the
// machine wakes from sleep - a machine that was off or asleep missed every
// notification sent meanwhile, and nothing else would tell it so.
//
// Installing is file-first. launchd loads every plist in
// ~/Library/LaunchAgents at login, and enabling a systemd unit is nothing
// more than the default.target.wants symlink `systemctl --user enable` would
// make, so the files alone are enough from the next login on. Starting it
// right away goes through launchctl/systemctl, but only as a best effort:
// over ssh there may be no GUI session or no user bus, and a failure there
// just means it starts at the next login.

// ServiceLabel names the unit on both platforms.
const ServiceLabel = "com.grillermo.git-sync"

// serviceFiles is where the unit lives on goos and what it says. link, when
// set, is the symlink that enables it, pointing at path.
type serviceFiles struct {
	path, content, link string
}

func serviceFor(goos, home, bin, logPath string) (serviceFiles, error) {
	switch goos {
	case "darwin":
		return serviceFiles{
			path:    filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist"),
			content: fmt.Sprintf(launchAgent, ServiceLabel, xmlEscape(bin), xmlEscape(logPath), xmlEscape(logPath)),
		}, nil
	case "linux":
		dir := filepath.Join(systemdConfigHome(home), "systemd", "user")
		return serviceFiles{
			path:    filepath.Join(dir, "git-sync.service"),
			content: fmt.Sprintf(systemdUnit, systemdQuote(bin)),
			link:    filepath.Join(dir, "default.target.wants", "git-sync.service"),
		}, nil
	}
	return serviceFiles{}, fmt.Errorf("no login service for %s (only macOS and Linux)", goos)
}

const launchAgent = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Installed by git-sync. Removed by 'git-sync service uninstall'. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>watch</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<!-- Restart only on failure: watch exits non-zero to be restarted on an
	     updated binary, and 0 once it has been uninstalled. -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`

// No network-online.target: a user unit cannot order itself after a system
// target. announce waits for the network itself. Restart=on-failure for the
// same reason as the plist's KeepAlive.
const systemdUnit = `# Installed by git-sync. Removed by 'git-sync service uninstall'.
[Unit]
Description=git-sync: tell the mesh this machine is back, at login and on wake

[Service]
Type=simple
ExecStart=%s watch
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`

func systemdConfigHome(home string) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	return filepath.Join(home, ".config")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func localService() (serviceFiles, error) {
	return serviceFor(runtime.GOOS, os.Getenv("HOME"), config.BinPath(), config.DebugLogPath())
}

// InstallService installs the login service on this machine, pointing at the
// installed binary. Idempotent.
func InstallService(out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	s, err := localService()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, []byte(s.content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", s.path, err)
	}
	if s.link != "" {
		if err := os.MkdirAll(filepath.Dir(s.link), 0o755); err != nil {
			return err
		}
		_ = os.Remove(s.link)
		if err := os.Symlink(s.path, s.link); err != nil {
			return fmt.Errorf("enabling %s: %w", s.path, err)
		}
	}
	fmt.Fprintf(out, "installed login service %s\n", s.path)
	if err := startService(); err != nil {
		fmt.Fprintf(out, "  (not started now: %v; it starts at the next login)\n", err)
	}
	return nil
}

// UninstallService removes the login service from this machine, if present.
func UninstallService(out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	s, err := localService()
	if err != nil {
		return nil // nothing could have been installed
	}
	// Stop it first. Not strictly needed - watch exits by itself once its
	// unit is gone - but it should not outlive the uninstall by a tick.
	stopService()
	removed := false
	for _, p := range []string{s.link, s.path} {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err == nil {
			removed = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if removed {
		fmt.Fprintf(out, "removed login service %s\n", s.path)
	}
	return nil
}

// startService (re)starts the installed service now, through the service
// manager. Re-starting matters on an upgrade: the running watch is still the
// old binary.
func startService() error {
	switch runtime.GOOS {
	case "darwin":
		s, err := localService()
		if err != nil {
			return err
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		_ = exec.Command("launchctl", "bootout", domain+"/"+ServiceLabel).Run()
		return run("launchctl", "bootstrap", domain, s.path)
	case "linux":
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return run("systemctl", "--user", "restart", "git-sync.service")
	}
	return nil
}

func stopService() {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), ServiceLabel)).Run()
	case "linux":
		_ = exec.Command("systemctl", "--user", "stop", "git-sync.service").Run()
	}
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ServiceInstalled reports whether this machine has the login service, and
// where it is (or would be).
func ServiceInstalled() (string, bool) {
	s, err := localService()
	if err != nil {
		return "", false
	}
	_, err = os.Stat(s.path)
	return s.path, err == nil
}

// ServiceMesh installs (or removes) the login service on every machine in
// the mesh, this one included. Installing also brings each machine's
// git-sync binary up to this one's version, since the service runs a
// subcommand an older binary does not have. An unreachable peer is a
// warning: it can be finished later by running the same command again.
func ServiceMesh(cfg config.Config, self string, remove bool, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if !remove {
		if err := os.MkdirAll(filepath.Dir(config.BinPath()), 0o755); err != nil {
			return err
		}
		if !sameFile(self, config.BinPath()) {
			if err := copyExecutable(self, config.BinPath()); err != nil {
				return fmt.Errorf("updating the installed binary: %w", err)
			}
		}
		if err := InstallService(out); err != nil {
			return err
		}
	} else if err := UninstallService(out); err != nil {
		return err
	}

	var failed []string
	for _, p := range cfg.PeerList() {
		var err error
		if remove {
			err = ssh(p.Target(), "~/.gitsync/bin/git-sync service uninstall --local")
		} else {
			err = installPeerService(p, self)
		}
		switch {
		case err == nil:
			verb := "installed the login service on"
			if remove {
				verb = "removed the login service from"
			}
			fmt.Fprintf(out, "%s %s\n", verb, p.Host)
		case IsPeerUnreachable(err):
			fmt.Fprintf(out, "WARNING: %s unreachable; run this again once it is up\n", p.Host)
		default:
			fmt.Fprintf(out, "WARNING: %s: %v\n", p.Host, err)
			failed = append(failed, p.Host)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed on %d of %d peer(s): %s", len(failed), len(cfg.PeerList()), strings.Join(failed, ", "))
	}
	return nil
}

// installPeerService sends a peer the binary for its platform, then has that
// binary install the service there - so the unit is written by code that
// knows the peer's own OS and paths.
func installPeerService(p config.Peer, self string) error {
	target := p.Target()
	probe, err := Probe(target)
	if err != nil {
		return err
	}
	o := PeerOptions{Self: self, Builds: BuildsDir(self)}
	binPath, err := o.binaryFor(probe.Uname)
	if err != nil {
		return err
	}
	if err := sendBinary(target, probe.Home+"/.gitsync", binPath); err != nil {
		return err
	}
	return ssh(target, probe.Home+"/.gitsync/bin/git-sync service install --local")
}

// BuildsDir is where per-platform builds sit: beside the binary this was run
// from (bin/ in the repo), not beside the installed copy.
func BuildsDir(self string) string {
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		return filepath.Dir(resolved)
	}
	return ""
}

func sameFile(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}
