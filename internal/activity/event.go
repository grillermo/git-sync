// Package activity is git-sync's structured, append-only event log. It is the
// only data source for `git-sync report`.
package activity

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// Op is what git-sync was doing.
type Op string

const (
	OpHook     Op = "hook"     // a commit fired the hook
	OpPush     Op = "push"     // pushing to the shared remote
	OpNotify   Op = "notify"   // telling the peer over ssh
	OpReceive  Op = "receive"  // applying what the peer pushed
	OpActivate Op = "activate" // running a repo's ./activate after a sync
)

// Status is how it turned out.
type Status string

const (
	StatusOK      Status = "ok"      // it worked
	StatusSkip    Status = "skip"    // deliberately did nothing; not a problem
	StatusWarn    Status = "warn"    // needs a human eventually (diverged, conflict)
	StatusError   Status = "error"   // it failed
	StatusOffline Status = "offline" // a machine was out of reach; queued and retried, not a problem
)

// Event is one line of activity.jsonl. Keep the json tags short: lines must
// stay under MaxLineLen for appends to be atomic.
type Event struct {
	Time   time.Time `json:"ts"`
	Repo   string    `json:"repo"`
	Op     Op        `json:"op"`
	Status Status    `json:"status"`
	Msg    string    `json:"msg,omitempty"`
	Branch string    `json:"branch,omitempty"`
	Peer   string    `json:"peer,omitempty"`
	// Run groups the events one operation writes (a receive logs several),
	// so a reader can tell a problem in the latest run from an older one.
	// Empty means the event is a run of its own.
	Run string `json:"run,omitempty"`
}

// IsProblem reports whether this event is one a user needs to know about.
// Skips are routine (a repo the peer lacks, nothing to fast-forward).
func (e Event) IsProblem() bool {
	return e.Status == StatusWarn || e.Status == StatusError
}

// NewRun returns a fresh id for Event.Run: unique per call, and per process
// across machines' worth of processes, because it includes the pid and the
// clock.
func NewRun() string {
	return fmt.Sprintf("%d.%d.%d", os.Getpid(), time.Now().UnixNano(), runSeq.Add(1))
}

// runSeq keeps two calls in one process distinct even when the clock's
// resolution is coarser than the gap between them.
var runSeq atomic.Uint64
