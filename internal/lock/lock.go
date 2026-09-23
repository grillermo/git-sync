// Package lock serialises concurrent syncs of the same repo. Rapid
// consecutive commits on the pusher fire overlapping receives, which would
// otherwise race their stash/fetch/merge steps against each other.
package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/config"
)

// StaleAfter is how old a lock must be before a new run assumes the holder
// died - an ssh drop, a kill -9 - and reclaims it.
const StaleAfter = 5 * time.Minute

// DefaultTimeout is how long to wait for a busy lock before giving up.
const DefaultTimeout = 30 * time.Second

const pollInterval = 100 * time.Millisecond

var errBusy = errors.New("sync already in progress")

// IsBusy reports whether Acquire gave up because another run holds the lock.
// Dropping the run is safe: fetch, merge and stash are idempotent against
// whatever state exists, so the holder brings the repo fully up to date.
func IsBusy(err error) bool { return errors.Is(err, errBusy) }

type Lock struct{ dir string }

func (l *Lock) Dir() string { return l.dir }

// Release removes the lock. Safe to call twice; always defer it.
func (l *Lock) Release() {
	if l == nil || l.dir == "" {
		return
	}
	_ = os.RemoveAll(l.dir)
}

// Owner is who holds a lock. Written into the lock directory so the commit
// hooks can tell the user which machine is mid-sync, rather than just
// refusing.
type Owner struct {
	From    string    `json:"from,omitempty"` // the notifying machine
	Started time.Time `json:"started"`
	PID     int       `json:"pid,omitempty"`
}

func (o Owner) Age() time.Duration { return time.Since(o.Started) }

const ownerFile = "owner"

// Acquire takes the lock for rel with no recorded origin.
func Acquire(rel string, timeout time.Duration) (*Lock, error) {
	return AcquireFrom(rel, "", timeout)
}

// AcquireFrom takes the lock and records which machine's notification is
// being processed.
func AcquireFrom(rel, from string, timeout time.Duration) (*Lock, error) {
	l, err := acquireDir(rel, timeout)
	if err != nil {
		return nil, err
	}
	l.writeOwner(Owner{From: from, Started: time.Now(), PID: os.Getpid()})
	return l, nil
}

func (l *Lock) writeOwner(o Owner) {
	// Best effort: a lock with no readable owner still blocks, it just
	// cannot name the machine.
	if b, err := json.Marshal(o); err == nil {
		_ = os.WriteFile(filepath.Join(l.dir, ownerFile), b, 0o644)
	}
}

// Refresh restamps the lock so a long sync is not mistaken for a dead one.
func (l *Lock) Refresh() error {
	if l == nil || l.dir == "" {
		return nil
	}
	now := time.Now()
	return os.Chtimes(l.dir, now, now)
}

// Held reports whether rel is locked right now. A stale lock is not held:
// the holder is assumed dead, and nothing should be blocked on it.
func Held(rel string) (Owner, bool) {
	dir := lockDir(rel)
	fi, err := os.Stat(dir)
	if err != nil || time.Since(fi.ModTime()) > StaleAfter {
		return Owner{}, false
	}
	var o Owner
	if b, err := os.ReadFile(filepath.Join(dir, ownerFile)); err == nil {
		_ = json.Unmarshal(b, &o) // an unreadable record leaves a zero Owner
	}
	if o.Started.IsZero() {
		o.Started = fi.ModTime()
	}
	return o, true
}

// Break removes a lock by hand, reporting who held it.
func Break(rel string) (Owner, bool, error) {
	o, held := Held(rel)
	dir := lockDir(rel)
	if _, err := os.Stat(dir); err != nil {
		return o, false, nil
	}
	return o, held, os.RemoveAll(dir)
}

// acquireDir takes the lock for rel, waiting up to timeout.
//
// The lock is a directory because mkdir is atomic on every filesystem we care
// about, including over NFS - unlike "check then create" on a file.
func acquireDir(rel string, timeout time.Duration) (*Lock, error) {
	dir := lockDir(rel)
	if err := os.MkdirAll(config.LocksDir(), 0o755); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	staleCleared := false
	for {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return &Lock{dir: dir}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}

		// Reclaim at most once per call, so a filesystem that refuses the
		// remove cannot spin us forever.
		if !staleCleared && isStale(dir) {
			staleCleared = true
			_ = os.RemoveAll(dir)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w for %s", errBusy, rel)
		}
		time.Sleep(pollInterval)
	}
}

func isStale(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) > StaleAfter
}

// lockDir flattens a relpath into a single directory name.
func lockDir(rel string) string {
	name := strings.ReplaceAll(filepath.ToSlash(rel), "/", "_")
	return filepath.Join(config.LocksDir(), name+".lock")
}
