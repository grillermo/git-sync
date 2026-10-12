package setup_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestUpdateMeshSendsTheNewBinaryEverywhere(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"uname": localUname(),
		"$HOME": "/home/peer",
	}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-new", "#!/bin/sh\necho new\n")
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "b.local", User: "t"}}}

	var out strings.Builder
	if err := setup.UpdateMesh(cfg, self, &out); err != nil {
		t.Fatalf("UpdateMesh: %v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(config.BinPath()); !strings.Contains(string(b), "echo new") {
		t.Error("this machine's installed binary was not updated")
	}
	if got := sb.SSHStdin(t, "git-sync.tmp"); !strings.Contains(got, "echo new") {
		t.Errorf("peer was not sent this binary: %q", got)
	}
	if strings.Contains(sb.SSHCalls(), "service install") {
		t.Errorf("update-mesh must only replace the binary:\n%s", sb.SSHCalls())
	}
	if _, ok := setup.ServiceInstalled(); ok {
		t.Error("update-mesh must not install a service that was not there")
	}
}

func TestUpdateMeshTreatsAnUnreachablePeerAsAWarning(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(255)
	self := testutil.WriteScript(t, sb, "git-sync-new", "#!/bin/sh\n")
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "down.local", User: "t"}}}

	var out strings.Builder
	if err := setup.UpdateMesh(cfg, self, &out); err != nil {
		t.Fatalf("an offline peer must not fail the command: %v", err)
	}
	if !strings.Contains(out.String(), "down.local unreachable") {
		t.Errorf("want a warning naming the peer:\n%s", out.String())
	}
}

func TestUpdateMeshFailsForAPeerWithNoBuild(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"uname": "Plan9 mips",
		"$HOME": "/home/peer",
	}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-new", "#!/bin/sh\n")
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "odd.local", User: "t"}}}

	var out strings.Builder
	if err := setup.UpdateMesh(cfg, self, &out); err == nil || !strings.Contains(err.Error(), "odd.local") {
		t.Errorf("UpdateMesh = %v, want a failure naming odd.local", err)
	}
}

func TestUpdateMeshFromTheInstalledCopySaysSo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	if err := os.MkdirAll(filepath.Dir(config.BinPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.BinPath(), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := setup.UpdateMesh(config.Config{BaseDir: sb.BaseDir}, config.BinPath(), &out); err != nil {
		t.Fatalf("UpdateMesh: %v", err)
	}
	if !strings.Contains(out.String(), "installed copy") {
		t.Errorf("want a note that nothing new was sent:\n%s", out.String())
	}
}

// TestUpdateMeshSendsALinuxPeerItsOwnBuild runs update-mesh the way `make
// update-mesh` does - from bin/git-sync, a symlink to this platform's build
// - and checks a Linux peer gets the Linux build beside it, not this binary.
func TestUpdateMeshSendsALinuxPeerItsOwnBuild(t *testing.T) {
	sb := testutil.NewSandbox(t)
	uname, goarch := "Linux x86_64", "amd64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		uname, goarch = "Linux aarch64", "arm64"
	}
	sb.StubSSHScripted(map[string]string{
		"uname": uname,
		"$HOME": "/home/grillermo",
	}, 0)

	bin := filepath.Join(sb.Home, "repo", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	own := "git-sync-" + runtime.GOOS + "-" + runtime.GOARCH
	if err := os.WriteFile(filepath.Join(bin, own), []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git-sync-linux-"+goarch), []byte("ELF linux build\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(bin, "git-sync")
	if err := os.Symlink(own, self); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: []config.Peer{{Host: "pc", User: "grillermo"}}}

	var out strings.Builder
	if err := setup.UpdateMesh(cfg, self, &out); err != nil {
		t.Fatalf("UpdateMesh: %v\n%s", err, out.String())
	}
	if got := sb.SSHStdin(t, "git-sync.tmp"); !strings.Contains(got, "ELF linux build") {
		t.Errorf("the Linux peer got %q, want the linux/%s build", got, goarch)
	}
	if b, _ := os.ReadFile(config.BinPath()); !strings.Contains(string(b), "echo mine") {
		t.Error("this machine must keep its own platform's build")
	}
}
