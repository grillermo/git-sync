// Package lock serialises concurrent syncs of the same repo. Rapid
// consecutive commits on the pusher fire overlapping receives, which would
// otherwise race their stash/fetch/merge steps against each other.
package lock

import (
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
	_ = os.Remove(l.dir)
}

// Acquire takes the lock for rel, waiting up to timeout.
//
// The lock is a directory because mkdir is atomic on every filesystem we care
// about, including over NFS - unlike "check then create" on a file.
func Acquire(rel string, timeout time.Duration) (*Lock, error) {
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
			_ = os.Remove(dir)
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
