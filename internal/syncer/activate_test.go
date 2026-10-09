package syncer_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
