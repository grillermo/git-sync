package syncer

import (
	"os"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
)

// ExitRestart is what Watch exits with when the installed binary has been
// replaced. Non-zero on purpose: the service is set to restart only on
// failure, so this brings it back up on the new binary, while a clean exit
// (uninstalled) leaves it stopped.
const ExitRestart = 75

// WatchOptions configures Watch. The zero value of every func field means
// the real thing; tests replace them to drive a fake clock.
type WatchOptions struct {
	// Interval is how often the clock is checked; a wake is noticed within
	// about this long. Slack is how far past Interval the wall clock must
	// jump to count as a sleep rather than a busy machine.
	Interval, Slack time.Duration

	// BinPath is the installed binary. Watch exits ExitRestart when it is
	// replaced, and 0 when it is gone.
	BinPath string

	// StillWanted reports whether the service is still installed. Watch exits
	// 0 once it is not.
	StillWanted func() bool

	Now      func() time.Time
	Sleep    func(time.Duration)
	Announce func(stop <-chan struct{})

	// Stop ends Watch with exit 0 at the next safe point: at once when
	// idle, and between steps when announcing.
	Stop <-chan struct{}
}

// Watch is the long-running process behind the login service: it announces
// this machine once at start (login), and again every time the machine wakes
// from sleep.
//
// Neither OS gives a user-level service a cgo-free sleep notification -
// macOS only via IOKit, Linux only via root's system-sleep hooks - so a wake
// is read off the clocks instead. The sleep between checks runs on the
// monotonic clock, which stops while the machine is asleep, while the wall
// clock keeps going. So a wall-clock gap far beyond Interval means the
// machine was asleep in between. A manual clock change can look the same;
// the cost is one extra, harmless announce.
func Watch(o WatchOptions) int {
	o.defaults()
	binInfo, err := os.Stat(o.BinPath)
	if err != nil {
		return 0
	}

	o.Announce(o.Stop)
	last := o.Now()
	for {
		o.Sleep(o.Interval)
		if stopped(o.Stop) {
			return 0
		}

		if !o.StillWanted() {
			return 0
		}
		switch fi, err := os.Stat(o.BinPath); {
		case err != nil:
			return 0
		case !os.SameFile(binInfo, fi):
			activity.AppendDebug("watch: git-sync was updated, restarting")
			return ExitRestart
		}

		now := o.Now()
		if now.Sub(last) > o.Interval+o.Slack {
			activity.AppendDebug("watch: woke after " + now.Sub(last).Round(time.Second).String() + ", announcing")
			o.Announce(o.Stop)
			now = o.Now()
		}
		last = now
	}
}

func (o *WatchOptions) defaults() {
	if o.Interval == 0 {
		o.Interval = 15 * time.Second
	}
	if o.Slack == 0 {
		o.Slack = time.Minute
	}
	if o.StillWanted == nil {
		o.StillWanted = func() bool { return true }
	}
	if o.Now == nil {
		// Round(0) strips the monotonic reading, so Sub compares wall
		// clocks - the one that keeps counting while the machine sleeps.
		o.Now = func() time.Time { return time.Now().Round(0) }
	}
	if o.Sleep == nil {
		stop := o.Stop
		o.Sleep = func(d time.Duration) {
			select {
			case <-stop:
			case <-time.After(d):
			}
		}
	}
	if o.Announce == nil {
		o.Announce = func(stop <-chan struct{}) { announce(stop) }
	}
}
