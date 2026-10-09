package syncer_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/lock"
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
	t.Setenv("GITSYNC_ACTIVATE_TIMEOUT", "1s")
	// The child sleep is what must die too, not just the shell.
	writeActivate(t, repo, `sleep 30 & echo $! > "$HOME/pid"; wait`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	start := time.Now()
	syncer.DrainActivate()
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("drain took %v; the timeout did not stop the script", took)
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusError, "timed out")
	b, err := os.ReadFile(filepath.Join(sb.Home, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatalf("child process %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
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

func TestDrainersNeverRunTwoActivatesAtOnce(t *testing.T) {
	sb := testutil.NewSandbox(t)
	for _, rel := range []string{"a", "b", "c"} {
		dir := sb.MakeRepo(rel)
		writeActivate(t, dir, `echo start >> "$HOME/runs"; sleep 0.2; echo end >> "$HOME/runs"`)
	}
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	for _, rel := range []string{"a", "b", "c"} {
		_ = syncer.EnqueueActivate(rel, "r")
	}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); syncer.DrainActivate() }()
	}
	wg.Wait()

	b, err := os.ReadFile(filepath.Join(sb.Home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), strings.Repeat("start\nend\n", 3); got != want {
		t.Errorf("runs overlapped or went missing:\n%s\nwant:\n%s", got, want)
	}
}

func TestASyncLandingMidRunActivatesAgainAfter(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `echo run >> "$HOME/runs"; touch "$HOME/started"; sleep 0.5`)
	_ = syncer.EnqueueActivate("group/proj", "r1")

	done := make(chan int)
	go func() { done <- syncer.DrainActivate() }()
	waitForFile(t, filepath.Join(sb.Home, "started"), 5*time.Second)
	if err := syncer.EnqueueActivate("group/proj", "r2"); err != nil {
		t.Fatal(err)
	}
	<-done

	b, _ := os.ReadFile(filepath.Join(sb.Home, "runs"))
	if n := strings.Count(string(b), "run\n"); n != 2 {
		t.Errorf("./activate ran %d times, want 2 (once more for the mid-run sync)", n)
	}
}

func TestDrainPostponesARepoThatIsMidReceive(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	t.Setenv("GITSYNC_LOCK_TIMEOUT", "50ms")
	writeActivate(t, repo, `touch "$HOME/ran"`)
	l, err := lock.AcquireFrom("group/proj", "peer.example", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_ = syncer.EnqueueActivate("group/proj", "oldrev")

	if code := syncer.DrainActivate(); code != 0 {
		t.Fatalf("DrainActivate = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(sb.Home, "ran")); err == nil {
		t.Error("ran ./activate under a receive")
	}
	q := syncer.ActivateQueue()
	if len(q) != 1 || q[0].Rel != "group/proj" || q[0].OldRev != "oldrev" {
		t.Errorf("queue = %+v, want the postponed repo still queued from oldrev", q)
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusSkip, "postponed")
}

func TestActivateNowTreatsEveryFileAsChanged(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, `git diff --name-only "$GITSYNC_OLD_REV" "$GITSYNC_NEW_REV"`)
	_ = syncer.EnqueueActivate("group/proj", "r")

	var out bytes.Buffer
	if code := syncer.ActivateNow("group/proj", &out); code != 0 {
		t.Fatalf("ActivateNow = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "README.md") {
		t.Errorf("output should list every tracked file, got:\n%s", out.String())
	}
	if syncer.HasActivateQueue() {
		t.Error("a manual run covers the queued entry; it should be gone")
	}
	testutil.AssertEvent(t, activity.OpActivate, activity.StatusOK, "everything")
}

func TestActivateNowRefusesARepoWithoutActivate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	var out bytes.Buffer
	if code := syncer.ActivateNow("group/proj", &out); code != 1 {
		t.Fatalf("ActivateNow = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "no executable ./activate") {
		t.Errorf("unhelpful message: %q", out.String())
	}
}

func TestActivateNowSaysWhyAFailingRunFailed(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, "exit 3")

	var out bytes.Buffer
	if code := syncer.ActivateNow("group/proj", &out); code != 1 {
		t.Fatalf("ActivateNow = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "activate: ./activate failed") {
		t.Errorf("terminal got no summary line: %q", out.String())
	}
}

func TestActivateNowCancelKillsTheScriptAndReleasesLocks(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	started := filepath.Join(t.TempDir(), "started")
	pidFile := filepath.Join(t.TempDir(), "pid")
	writeActivate(t, repo, "echo $$ > "+pidFile+"\ntouch "+started+"\nsleep 60")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	var out bytes.Buffer
	go func() { done <- syncer.ActivateNowContext(ctx, "group/proj", &out) }()
	waitForFile(t, started, 5*time.Second)
	cancel()

	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("code = %d, want 1", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ActivateNowContext did not return after cancel")
	}
	if !strings.Contains(out.String(), "interrupted") {
		t.Errorf("no interrupted summary: %q", out.String())
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("repo lock leaked after cancel")
	}
	// the drain lock must be free too
	l, err := lock.Acquire(".activate", 0)
	if err != nil {
		t.Fatalf("drain lock leaked: %v", err)
	}
	l.Release()
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	time.Sleep(100 * time.Millisecond)
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		t.Errorf("script %d still running", pid)
	}
}

func TestActivateNowIsInterruptibleWhileWaitingForTheDrainLock(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	writeActivate(t, repo, "true")
	held, err := lock.Acquire(".activate", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	done := make(chan int)
	var out bytes.Buffer
	go func() { done <- syncer.ActivateNowContext(ctx, "group/proj", &out) }()
	select {
	case code := <-done:
		if code != 130 {
			t.Errorf("code = %d, want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting for the lock after cancel")
	}
	if _, still := lock.Held(".activate"); !still {
		t.Error("released a drain lock it never took")
	}
	if _, h := lock.Held("group/proj"); h {
		t.Error("repo lock taken or leaked")
	}
}
