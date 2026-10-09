package syncer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/syncer"
)

// fakeClock drives Watch: each Sleep advances wall time by the slept
// duration plus whatever the script says the machine spent asleep.
type fakeClock struct {
	now    time.Time
	asleep []time.Duration // extra wall time per tick
	tick   int
	onTick func(tick int)
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) {
	c.now = c.now.Add(d)
	if c.tick < len(c.asleep) {
		c.now = c.now.Add(c.asleep[c.tick])
	}
	c.tick++
	if c.onTick != nil {
		c.onTick(c.tick)
	}
}

func fakeBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "git-sync")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// wantedFor keeps the service "installed" for n ticks.
func wantedFor(c *fakeClock, n int) func() bool {
	return func() bool { return c.tick <= n }
}

func TestWatchAnnouncesAtStartAndOnEveryWake(t *testing.T) {
	c := &fakeClock{now: time.Unix(1e9, 0), asleep: []time.Duration{0, 8 * time.Hour, 0, 0, 20 * time.Minute, 0}}
	announces := 0
	code := syncer.Watch(syncer.WatchOptions{
		Interval: 15 * time.Second, Slack: time.Minute,
		BinPath: fakeBin(t), StillWanted: wantedFor(c, 6),
		Now: c.Now, Sleep: c.Sleep,
		Announce: func(<-chan struct{}) { announces++ },
	})
	if code != 0 {
		t.Errorf("Watch = %d, want 0 once uninstalled", code)
	}
	if announces != 3 {
		t.Errorf("announced %d times, want 3 (start + two wakes)", announces)
	}
}

func TestWatchIgnoresASlowTick(t *testing.T) {
	// A loaded machine oversleeping by seconds is not a wake.
	c := &fakeClock{now: time.Unix(1e9, 0), asleep: []time.Duration{20 * time.Second, 45 * time.Second}}
	announces := 0
	syncer.Watch(syncer.WatchOptions{
		Interval: 15 * time.Second, Slack: time.Minute,
		BinPath: fakeBin(t), StillWanted: wantedFor(c, 2),
		Now: c.Now, Sleep: c.Sleep,
		Announce: func(<-chan struct{}) { announces++ },
	})
	if announces != 1 {
		t.Errorf("announced %d times, want only the one at start", announces)
	}
}

func TestWatchRestartsWhenTheBinaryIsReplaced(t *testing.T) {
	bin := fakeBin(t)
	c := &fakeClock{now: time.Unix(1e9, 0)}
	c.onTick = func(tick int) {
		if tick == 2 { // install renames a new binary into place
			tmp := bin + ".tmp"
			_ = os.WriteFile(tmp, []byte("v2"), 0o755)
			_ = os.Rename(tmp, bin)
		}
	}
	code := syncer.Watch(syncer.WatchOptions{
		BinPath: bin, StillWanted: wantedFor(c, 10),
		Now: c.Now, Sleep: c.Sleep, Announce: func(<-chan struct{}) {},
	})
	if code != syncer.ExitRestart {
		t.Errorf("Watch = %d, want ExitRestart so the service restarts on the new binary", code)
	}
	if c.tick != 2 {
		t.Errorf("noticed after %d ticks, want 2", c.tick)
	}
}

func TestWatchExitsCleanlyWhenTheBinaryIsGone(t *testing.T) {
	bin := fakeBin(t)
	c := &fakeClock{now: time.Unix(1e9, 0)}
	c.onTick = func(int) { _ = os.Remove(bin) }
	code := syncer.Watch(syncer.WatchOptions{
		BinPath: bin, StillWanted: wantedFor(c, 10),
		Now: c.Now, Sleep: c.Sleep, Announce: func(<-chan struct{}) {},
	})
	if code != 0 {
		t.Errorf("Watch = %d, want 0 (uninstalled, do not restart)", code)
	}
}

func TestWatchStopsOnSignal(t *testing.T) {
	stop := make(chan struct{})
	c := &fakeClock{now: time.Unix(1e9, 0)}
	c.onTick = func(tick int) {
		if tick == 3 {
			close(stop)
		}
	}
	var gotStop <-chan struct{}
	code := syncer.Watch(syncer.WatchOptions{
		BinPath: fakeBin(t), StillWanted: func() bool { return true },
		Now: c.Now, Sleep: c.Sleep, Stop: stop,
		Announce: func(s <-chan struct{}) { gotStop = s },
	})
	if code != 0 || c.tick != 3 {
		t.Errorf("Watch = %d after %d ticks, want 0 after 3", code, c.tick)
	}
	if gotStop != (<-chan struct{})(stop) {
		t.Error("announce was not given the stop channel, so a stop would wait out its retries")
	}
}

func TestWatchUsesRealTimeByDefault(t *testing.T) {
	// The defaults wire up real sleeping; a stop must still end it promptly.
	stop := make(chan struct{})
	done := make(chan int)
	go func() {
		done <- syncer.Watch(syncer.WatchOptions{
			Interval: time.Hour, BinPath: fakeBin(t), Stop: stop,
			Announce: func(<-chan struct{}) {},
		})
	}()
	close(stop)
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("Watch = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stopped Watch kept sleeping")
	}
}

func TestWatchRunsAfterAnnounceAfterEveryAnnounce(t *testing.T) {
	c := &fakeClock{now: time.Unix(1e9, 0), asleep: []time.Duration{0, 8 * time.Hour, 0}}
	var order []string
	syncer.Watch(syncer.WatchOptions{
		Interval: 15 * time.Second, Slack: time.Minute,
		BinPath: fakeBin(t), StillWanted: wantedFor(c, 3),
		Now: c.Now, Sleep: c.Sleep,
		Announce:      func(<-chan struct{}) { order = append(order, "announce") },
		AfterAnnounce: func() { order = append(order, "after") },
	})
	if got := strings.Join(order, ","); got != "announce,after,announce,after" {
		t.Errorf("order = %s, want announce,after twice", got)
	}
}
