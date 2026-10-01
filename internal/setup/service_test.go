package setup_test

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestTheMacServiceIsALaunchAgentThatRunsAnnounceAtLogin(t *testing.T) {
	path, content, link, err := setup.ServiceFor("darwin", "/Users/me", "/Users/me/.gitsync/bin/git-sync", "/Users/me/.gitsync/debug.log")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/Users/me/Library/LaunchAgents/com.grillermo.git-sync.plist" {
		t.Errorf("path = %s", path)
	}
	if link != "" {
		t.Errorf("launchd needs no enabling link, got %s", link)
	}
	for _, want := range []string{
		"<string>/Users/me/.gitsync/bin/git-sync</string>",
		"<string>watch</string>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<string>/Users/me/.gitsync/debug.log</string>",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plist missing %q:\n%s", want, content)
		}
	}
}

func TestTheLinuxServiceIsAnEnabledSystemdUserUnit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	path, content, link, err := setup.ServiceFor("linux", "/home/me", "/home/me/.gitsync/bin/git-sync", "")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/home/me/.config/systemd/user/git-sync.service" {
		t.Errorf("path = %s", path)
	}
	if link != "/home/me/.config/systemd/user/default.target.wants/git-sync.service" {
		t.Errorf("link = %s", link)
	}
	for _, want := range []string{"ExecStart=/home/me/.gitsync/bin/git-sync watch", "Type=simple", "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(content, want) {
			t.Errorf("unit missing %q:\n%s", want, content)
		}
	}
}

func TestTheLinuxServiceHonoursXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/elsewhere")
	path, _, _, _ := setup.ServiceFor("linux", "/home/me", "/bin/git-sync", "")
	if path != "/elsewhere/systemd/user/git-sync.service" {
		t.Errorf("path = %s", path)
	}
}

func TestThereIsNoServiceForOtherSystems(t *testing.T) {
	if _, _, _, err := setup.ServiceFor("windows", "C:/", "x", "y"); err == nil {
		t.Error("want an error for an unsupported OS")
	}
}

func TestInstallServiceWritesTheUnitAndUninstallRemovesIt(t *testing.T) {
	testutil.NewSandbox(t)
	if err := setup.InstallService(io.Discard); err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	path, ok := setup.ServiceInstalled()
	if !ok {
		t.Fatalf("service not installed at %s", path)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), config.BinPath()) {
		t.Errorf("unit does not run the installed binary %s:\n%s", config.BinPath(), b)
	}
	// Idempotent.
	if err := setup.InstallService(io.Discard); err != nil {
		t.Fatalf("second InstallService: %v", err)
	}

	if err := setup.UninstallService(io.Discard); err != nil {
		t.Fatalf("UninstallService: %v", err)
	}
	if _, ok := setup.ServiceInstalled(); ok {
		t.Error("service still installed")
	}
	if err := setup.UninstallService(io.Discard); err != nil {
		t.Errorf("removing an absent service: %v", err)
	}
}

func TestInstallSetsUpTheServiceHereAndOnThePeer(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"uname": localUname(), "$HOME": "/home/peer"}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "peer.example", User: "t"}},
		Self: self, Out: io.Discard,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := setup.ServiceInstalled(); !ok {
		t.Error("no service on this machine")
	}
	if !strings.Contains(sb.SSHCalls(), "/home/peer/.gitsync/bin/git-sync service install --local") {
		t.Errorf("peer's service never installed:\n%s", sb.SSHCalls())
	}
}

func TestInstallNoServiceSkipsItEverywhere(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"uname": localUname(), "$HOME": "/home/peer"}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "peer.example", User: "t"}},
		Self: self, NoService: true, Out: io.Discard,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, ok := setup.ServiceInstalled(); ok {
		t.Error("service installed despite NoService")
	}
	if strings.Contains(sb.SSHCalls(), "service install") {
		t.Error("peer service installed despite NoService")
	}
}

func TestUninstallRemovesTheService(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	_ = setup.Install(setup.Options{BaseDir: sb.BaseDir, NoPeer: true, Self: self, Out: io.Discard})

	if err := setup.Uninstall(false, io.Discard); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, ok := setup.ServiceInstalled(); ok {
		t.Error("service survived uninstall")
	}
}

func TestServiceMeshUpgradesEachMachineAndInstallsTheService(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"uname": localUname(),
		"$HOME": "/home/peer",
	}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-new", "#!/bin/sh\necho new\n")
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: []config.Peer{
		{Host: "up.local", User: "t"},
	}}

	var out strings.Builder
	if err := setup.ServiceMesh(cfg, self, false, &out); err != nil {
		t.Fatalf("ServiceMesh: %v\n%s", err, out.String())
	}

	if b, _ := os.ReadFile(config.BinPath()); !strings.Contains(string(b), "echo new") {
		t.Error("this machine's installed binary was not updated")
	}
	if _, ok := setup.ServiceInstalled(); !ok {
		t.Error("no service on this machine")
	}
	calls := sb.SSHCalls()
	for _, want := range []string{"/home/peer/.gitsync/bin/git-sync.tmp", "/home/peer/.gitsync/bin/git-sync service install --local"} {
		if !strings.Contains(calls, want) {
			t.Errorf("ssh calls missing %q:\n%s", want, calls)
		}
	}
	if got := sb.SSHStdin(t, "git-sync.tmp"); !strings.Contains(got, "echo new") {
		t.Errorf("peer was not sent this binary: %q", got)
	}
}

func TestServiceMeshTreatsAnUnreachablePeerAsAWarning(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(255)
	self := testutil.WriteScript(t, sb, "git-sync-new", "#!/bin/sh\n")
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "down.local", User: "t"}}}

	var out strings.Builder
	if err := setup.ServiceMesh(cfg, self, false, &out); err != nil {
		t.Fatalf("an offline peer must not fail the command: %v", err)
	}
	if !strings.Contains(out.String(), "down.local unreachable") {
		t.Errorf("want a warning naming the peer:\n%s", out.String())
	}
	if _, ok := setup.ServiceInstalled(); !ok {
		t.Error("this machine should still get the service")
	}
}

func TestServiceMeshUninstallRemovesItEverywhere(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	if err := setup.InstallService(io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "b.local", User: "t"}}}

	if err := setup.ServiceMesh(cfg, filepath.Join(sb.Home, "unused"), true, io.Discard); err != nil {
		t.Fatalf("ServiceMesh uninstall: %v", err)
	}
	if _, ok := setup.ServiceInstalled(); ok {
		t.Error("service still installed here")
	}
	if !strings.Contains(sb.SSHCalls(), "git-sync service uninstall --local") {
		t.Errorf("peer not asked to remove its service:\n%s", sb.SSHCalls())
	}
}

func TestInstallServiceStartsItNowAndUninstallStopsIt(t *testing.T) {
	sb := testutil.NewSandbox(t)
	if err := setup.InstallService(io.Discard); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"darwin": {"launchctl bootout gui/", "launchctl bootstrap gui/"},
		"linux":  {"systemctl --user daemon-reload", "systemctl --user restart git-sync.service"},
	}[runtime.GOOS]
	for _, w := range want {
		if !strings.Contains(sb.ServiceCalls(), w) {
			t.Errorf("service manager calls missing %q:\n%s", w, sb.ServiceCalls())
		}
	}

	before := sb.ServiceCalls()
	if err := setup.UninstallService(io.Discard); err != nil {
		t.Fatal(err)
	}
	stop := map[string]string{"darwin": "launchctl bootout", "linux": "systemctl --user stop"}[runtime.GOOS]
	if after := strings.TrimPrefix(sb.ServiceCalls(), before); !strings.Contains(after, stop) {
		t.Errorf("uninstall did not stop the service:\n%s", after)
	}
}

func TestAServiceManagerThatCannotStartItIsNotAnError(t *testing.T) {
	sb := testutil.NewSandbox(t)
	// Over ssh there may be no GUI session or user bus to talk to.
	for _, name := range []string{"launchctl", "systemctl"} {
		testutil.WriteFileIn(t, filepath.Join(sb.Home, "bin"), name, "#!/bin/sh\necho 'no session' >&2\nexit 1\n")
		_ = os.Chmod(filepath.Join(sb.Home, "bin", name), 0o755)
	}
	var out strings.Builder
	if err := setup.InstallService(&out); err != nil {
		t.Fatalf("InstallService: %v", err)
	}
	if _, ok := setup.ServiceInstalled(); !ok {
		t.Error("the unit should still be written, to start at the next login")
	}
	if !strings.Contains(out.String(), "starts at the next login") {
		t.Errorf("should say when it will start:\n%s", out.String())
	}
}
