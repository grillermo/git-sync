package lock_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/lock"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestAcquireAndRelease(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.Acquire("group/proj", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Stat(l.Dir()); err != nil {
		t.Errorf("lock dir should exist while held: %v", err)
	}
	l.Release()
	if _, err := os.Stat(l.Dir()); !os.IsNotExist(err) {
		t.Error("lock dir should be gone after Release")
	}
}

func TestRelpathIsFlattenedIntoOneLockName(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.Acquire("group/proj", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	want := filepath.Join(config.LocksDir(), "group_proj.lock")
	if l.Dir() != want {
		t.Errorf("lock dir = %q, want %q", l.Dir(), want)
	}
}

func TestAcquireTimesOutWhileHeld(t *testing.T) {
	testutil.NewSandbox(t)
	held, err := lock.Acquire("group/proj", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	start := time.Now()
	_, err = lock.Acquire("group/proj", 300*time.Millisecond)
	elapsed := time.Since(start)

	if !lock.IsBusy(err) {
		t.Fatalf("err = %v, want a busy error", err)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("gave up after %v, should have waited the full timeout", elapsed)
	}
	if _, statErr := os.Stat(held.Dir()); statErr != nil {
		t.Error("giving up must not delete the holder's lock")
	}
}

func TestAcquireSucceedsOnceReleased(t *testing.T) {
	testutil.NewSandbox(t)
	held, _ := lock.Acquire("group/proj", time.Second)
	go func() {
		time.Sleep(100 * time.Millisecond)
		held.Release()
	}()

	l, err := lock.Acquire("group/proj", 2*time.Second)
	if err != nil {
		t.Fatalf("Acquire should have waited out the holder: %v", err)
	}
	l.Release()
}

func TestStaleLockIsReclaimed(t *testing.T) {
	testutil.NewSandbox(t)
	dir := filepath.Join(config.LocksDir(), "group_proj.lock")
	testutil.MkdirAll(t, dir)
	// Backdate past the stale threshold: a lock left by a killed process,
	// an ssh drop or a kill -9.
	old := time.Now().Add(-2 * lock.StaleAfter)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	l, err := lock.Acquire("group/proj", 10*time.Second)
	if err != nil {
		t.Fatalf("a stale lock should be reclaimed, not waited out: %v", err)
	}
	defer l.Release()
	if time.Since(start) > 2*time.Second {
		t.Error("reclaiming a stale lock should be immediate")
	}
}

func TestStaleLockWithOwnerFileIsReclaimed(t *testing.T) {
	testutil.NewSandbox(t)
	// A real lock via AcquireFrom leaves an owner file in the lock dir,
	// unlike a hand-built directory. Reclaim must remove the whole
	// directory (os.RemoveAll), not just try to os.Remove an empty one -
	// which fails silently (ENOTEMPTY) and would permanently wedge the
	// lock in production.
	first, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	dir := first.Dir()
	old := time.Now().Add(-2 * lock.StaleAfter)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	start := time.Now()
	second, err := lock.Acquire("group/proj", 10*time.Second)
	if err != nil {
		t.Fatalf("a stale lock with an owner file should be reclaimed, not waited out: %v", err)
	}
	defer second.Release()
	if time.Since(start) > 2*time.Second {
		t.Error("reclaiming a stale lock should be immediate")
	}
}

func TestOnlyOneHolderAtATime(t *testing.T) {
	testutil.NewSandbox(t)
	var mu sync.Mutex
	concurrent, max := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := lock.Acquire("group/proj", 5*time.Second)
			if err != nil {
				return
			}
			defer l.Release()
			mu.Lock()
			concurrent++
			if concurrent > max {
				max = concurrent
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			concurrent--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if max != 1 {
		t.Errorf("%d goroutines held the lock at once, want 1", max)
	}
}

func TestHeldReportsTheOwner(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()

	owner, held := lock.Held("group/proj")
	if !held {
		t.Fatal("Held = false while the lock is taken")
	}
	if owner.From != "laptop.local" {
		t.Errorf("owner.From = %q, want laptop.local", owner.From)
	}
	if owner.Age() > time.Minute {
		t.Errorf("owner.Age() = %v, want a fresh lock", owner.Age())
	}
}

func TestHeldIsFalseWhenNoLockExists(t *testing.T) {
	testutil.NewSandbox(t)
	if _, held := lock.Held("group/proj"); held {
		t.Error("Held = true with no lock")
	}
}

func TestHeldIsFalseForAStaleLock(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()
	old := time.Now().Add(-2 * lock.StaleAfter)
	if err := os.Chtimes(l.Dir(), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("a stale lock must not block anything")
	}
}

func TestRefreshKeepsALockAlive(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()
	old := time.Now().Add(-2 * lock.StaleAfter)
	_ = os.Chtimes(l.Dir(), old, old)
	if err := l.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, held := lock.Held("group/proj"); !held {
		t.Error("a refreshed lock must still be held")
	}
}

// A process killed between mkdir and the owner write leaves a lock with no
// readable owner. It still blocks - something is in there - but it must not
// panic, and the stale timer must still apply.
func TestHeldSurvivesAMissingOrTruncatedOwnerFile(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()
	if err := os.WriteFile(filepath.Join(l.Dir(), "owner"), []byte("{\"from\":"), 0o644); err != nil {
		t.Fatalf("truncate owner: %v", err)
	}
	owner, held := lock.Held("group/proj")
	if !held {
		t.Fatal("a lock with an unreadable owner is still a lock")
	}
	if owner.From != "" {
		t.Errorf("owner.From = %q, want empty for an unreadable record", owner.From)
	}

	old := time.Now().Add(-2 * lock.StaleAfter)
	_ = os.Chtimes(l.Dir(), old, old)
	if _, held := lock.Held("group/proj"); held {
		t.Error("an unreadable owner must not make a lock un-expirable")
	}
}

func TestBreakRemovesTheLockAndReportsWhoHadIt(t *testing.T) {
	testutil.NewSandbox(t)
	l, err := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	_ = l

	owner, had, err := lock.Break("group/proj")
	if err != nil {
		t.Fatalf("Break: %v", err)
	}
	if !had || owner.From != "laptop.local" {
		t.Fatalf("Break = (%+v, %v), want the previous owner", owner, had)
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("Break left the lock in place")
	}
}
