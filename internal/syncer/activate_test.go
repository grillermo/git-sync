package syncer_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

// writeActivate puts an executable ./activate running body into repoDir.
func writeActivate(t *testing.T, repoDir, body string) {
	t.Helper()
	p := filepath.Join(repoDir, "activate")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// waitForFile polls until path exists or timeout passes.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %v", path, timeout)
}

func TestEnqueueActivateKeepsTheOldestRev(t *testing.T) {
	testutil.NewSandbox(t)
	if err := syncer.EnqueueActivate("group/proj", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := syncer.EnqueueActivate("group/proj", "bbb"); err != nil {
		t.Fatal(err)
	}
	q := syncer.ActivateQueue()
	if len(q) != 1 || q[0].Rel != "group/proj" || q[0].OldRev != "aaa" {
		t.Fatalf("queue = %+v, want one group/proj entry from aaa", q)
	}
}

func TestActivateQueueIsOldestFirstAndRoundTripsNames(t *testing.T) {
	testutil.NewSandbox(t)
	want := []string{"b", "group/a", ".dotfiles"}
	for _, rel := range want {
		if err := syncer.EnqueueActivate(rel, "r"); err != nil {
			t.Fatal(err)
		}
		// Order is by mtime; keep entries apart on coarse filesystems.
		time.Sleep(20 * time.Millisecond)
	}
	q := syncer.ActivateQueue()
	if len(q) != len(want) {
		t.Fatalf("queue = %+v, want %d entries", q, len(want))
	}
	for i, rel := range want {
		if q[i].Rel != rel {
			t.Errorf("queue[%d] = %q, want %q", i, q[i].Rel, rel)
		}
	}
}

func TestHasActivateQueue(t *testing.T) {
	testutil.NewSandbox(t)
	if syncer.HasActivateQueue() {
		t.Fatal("empty queue reported as non-empty")
	}
	_ = syncer.EnqueueActivate("group/proj", "r")
	if !syncer.HasActivateQueue() {
		t.Fatal("queued entry not reported")
	}
}
func TestDrainRunsActivateWithTheRevRange(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `echo "old=$GITSYNC_OLD_REV new=$GITSYNC_NEW_REV internal=$GITSYNC_INTERNAL." > "$HOME/ran"`)
	// git-sync's own marker must never reach user code.
	t.Setenv("GITSYNC_INTERNAL", "1")
	head := strings.TrimSpace(sb.Git(repo, "rev-parse", "HEAD"))
	if err := syncer.EnqueueActivate("group/proj", "abc123"); err != nil {
		t.Fatal(err)
	}

	if code := syncer.DrainActivate(); code != 0 {
		t.Fatalf("DrainActivate = %d, want 0", code)
	}
	testutil.AssertFileContains(t, filepath.Join(sb.Home, "ran"), "old=abc123 new="+head+" internal=.")
	if syncer.HasActivateQueue() {
		t.Error("queue should be empty after a drain")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusOK, "activated")
}

func TestDrainRecordsAFailingActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `echo boom-output; exit 3`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	if code := syncer.DrainActivate(); code != 0 {
		t.Fatalf("DrainActivate = %d, want 0 (a failed activate is an event, not a drain failure)", code)
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusError, "failed")
	testutil.AssertFileContains(t, syncer.ActivateLogPath("group/proj"), "boom-output")
}

func TestDrainKillsAnActivateThatRunsTooLong(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	t.Setenv("GITSYNC_ACTIVATE_TIMEOUT", "300ms")
	// The child sleep is what must die too, not just the shell.
	writeActivate(t, repo, `sleep 5 & wait`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	start := time.Now()
	syncer.DrainActivate()
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("drain took %v; the timeout did not stop the script", took)
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusError, "timed out")
}

func TestDrainDropsARepoNoLongerSelected(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	sb.MakeRepo("group/other")
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"group/other"})
	writeActivate(t, repo, `touch "$HOME/ran"`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	syncer.DrainActivate()
	if _, err := os.Stat(filepath.Join(sb.Home, "ran")); err == nil {
		t.Error("ran ./activate for a repo that is not selected")
	}
	if syncer.HasActivateQueue() {
		t.Error("entry should have been dropped")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusSkip, "dropped")
}

func TestDrainDropsARepoWithoutActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	_ = syncer.EnqueueActivate("group/proj", "r")

	syncer.DrainActivate()
	if syncer.HasActivateQueue() {
		t.Error("entry should have been dropped")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusSkip, "no ./activate")
}

func TestConcurrentEnqueuesOfOneRepoYieldOneEntry(t *testing.T) {
	testutil.NewSandbox(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = syncer.EnqueueActivate("group/proj", "r")
		}()
	}
	wg.Wait()
	if q := syncer.ActivateQueue(); len(q) != 1 {
		t.Fatalf("queue = %+v, want exactly one entry", q)
	}
}
