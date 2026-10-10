package status_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/status"
	"github.com/grillermo/git-sync/internal/testutil"
)

type lines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), "\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFollowPrintsAtStartAndOnEveryChange(t *testing.T) {
	var (
		mu sync.Mutex
		fp = "a"
	)
	out := &lines{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- status.Follow(ctx, out, status.FollowOptions{
			Collect:     func() status.Snapshot { return status.Snapshot{Repos: []status.Repo{}, Pending: []status.Pending{}} },
			Fingerprint: func() string { mu.Lock(); defer mu.Unlock(); return fp },
			Tick:        5 * time.Millisecond,
			Refresh:     time.Hour,
		})
	}()

	waitFor(t, "the first snapshot", func() bool { return out.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := out.count(); n != 1 {
		t.Fatalf("printed %d lines with nothing changed, want 1", n)
	}
	mu.Lock()
	fp = "b"
	mu.Unlock()
	waitFor(t, "a snapshot after the change", func() bool { return out.count() == 2 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Follow = %v, want nil on cancel", err)
	}
}

func TestFollowRefreshesWithoutAChange(t *testing.T) {
	out := &lines{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go status.Follow(ctx, out, status.FollowOptions{
		Collect:     func() status.Snapshot { return status.Snapshot{} },
		Fingerprint: func() string { return "same" },
		Tick:        5 * time.Millisecond,
		Refresh:     20 * time.Millisecond,
	})
	waitFor(t, "a refresh", func() bool { return out.count() >= 2 })
}

func TestFingerprintSeesAddedAndGrownFiles(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "log")
	before := status.Fingerprint(dir, filepath.Join(dir, "missing"))
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	added := status.Fingerprint(dir, filepath.Join(dir, "missing"))
	if added == before {
		t.Error("adding a file did not change the fingerprint")
	}
	if err := os.WriteFile(f, []byte("xy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status.Fingerprint(dir, filepath.Join(dir, "missing")) == added {
		t.Error("growing a file did not change the fingerprint")
	}
}

func TestCollectWithoutAConfigSaysNotInstalled(t *testing.T) {
	testutil.NewSandbox(t)
	s := status.Collect(time.Now())
	if !strings.Contains(s.Error, "not installed") || s.Repos == nil || s.Pending == nil {
		t.Errorf("got %+v", s)
	}
}

func TestWriteTextShowsReposAndPending(t *testing.T) {
	var b strings.Builder
	status.WriteText(&b, build(cfg("group/a")))
	if !strings.Contains(b.String(), "group/a") || !strings.Contains(b.String(), "never") {
		t.Errorf("text output:\n%s", b.String())
	}
}
