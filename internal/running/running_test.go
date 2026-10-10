package running_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestStartIsListedUntilStopped(t *testing.T) {
	testutil.NewSandbox(t)
	stop := running.Start("group/proj", activity.OpNotify, "peer.local")

	got := running.List()
	if len(got) != 1 {
		t.Fatalf("List = %+v, want one marker", got)
	}
	m := got[0]
	if m.Repo != "group/proj" || m.Op != activity.OpNotify || m.Peer != "peer.local" || m.PID != os.Getpid() || m.Started.IsZero() {
		t.Errorf("marker = %+v", m)
	}

	stop()
	stop() // idempotent
	if got := running.List(); len(got) != 0 {
		t.Errorf("after stop, List = %+v, want none", got)
	}
}

func TestTwoOperationsOfOneProcessAreBothListed(t *testing.T) {
	testutil.NewSandbox(t)
	defer running.Start("group/proj", activity.OpNotify, "a.local")()
	defer running.Start("group/proj", activity.OpNotify, "b.local")()
	if got := running.List(); len(got) != 2 {
		t.Errorf("List = %+v, want two markers", got)
	}
}

func TestListDropsAndRemovesMarkersOfDeadProcesses(t *testing.T) {
	testutil.NewSandbox(t)
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.ProcessState.Pid()
	testutil.MkdirAll(t, config.RunningDir())
	path := filepath.Join(config.RunningDir(), fmt.Sprintf("%d-x.json", dead))
	body := fmt.Sprintf(`{"repo":"r","op":"push","started":"2026-10-09T12:00:00Z","pid":%d}`, dead)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := running.List(); len(got) != 0 {
		t.Errorf("List = %+v, want the dead process's marker dropped", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a dead process's marker should be removed")
	}
}

func TestListSkipsUnreadableMarkers(t *testing.T) {
	testutil.NewSandbox(t)
	testutil.MkdirAll(t, config.RunningDir())
	// Our own pid, so it is alive, but the body is half-written.
	path := filepath.Join(config.RunningDir(), fmt.Sprintf("%d-y.json", os.Getpid()))
	if err := os.WriteFile(path, []byte(`{"repo":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := running.List(); len(got) != 0 {
		t.Errorf("List = %+v, want the torn marker skipped", got)
	}
}
