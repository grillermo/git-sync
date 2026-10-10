package syncer_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestActivateIsMarkedRunningWhileItRuns(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	seen := filepath.Join(sb.Home, "seen")
	script := "#!/bin/sh\ncat \"$GITSYNC_HOME\"/running/* > '" + seen + "'\n"
	if err := os.WriteFile(filepath.Join(repo, "activate"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if code := syncer.ActivateNow("group/proj", io.Discard); code != 0 {
		t.Fatalf("ActivateNow = %d, want 0", code)
	}
	testutil.AssertFileContains(t, seen, `"op":"activate"`)
	if m := running.List(); len(m) != 0 {
		t.Errorf("marker left behind: %+v", m)
	}
}

func TestReceiveIsMarkedRunningWhileItRuns(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	// A fake git first on PATH records the running dir at fetch time, then
	// hands over to the real git.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(sb.Home, "seen")
	testutil.WriteScript(t, sb, "git", "#!/bin/sh\n"+
		"[ \"$1\" = fetch ] && cat \"$GITSYNC_HOME\"/running/* > '"+seen+"'\n"+
		"exec '"+realGit+"' \"$@\"\n")
	t.Setenv("PATH", filepath.Join(sb.Home, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	syncer.Receive("group/proj", "peer.example")
	testutil.AssertFileContains(t, seen, `"op":"receive"`)
	testutil.AssertFileContains(t, seen, `"peer":"peer.example"`)
}

func TestPushAndReceiveLeaveNoRunningMarker(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	syncer.Receive("group/proj", "peer.example")
	entries, _ := os.ReadDir(config.RunningDir())
	if len(entries) != 0 {
		t.Errorf("running/ not empty after push and receive: %v", entries)
	}
}
