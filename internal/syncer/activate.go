package syncer

import (
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/config"
)

// A receive that moves a repo forward only records that the repo needs its
// ./activate run. A separate drainer runs the record later, one repo at a time
// per machine: a wake-up announce can land a dozen repos at once, and a dozen
// parallel Go builds and service restarts would swamp the machine.
//
// Each entry is a file named after the path-escaped rel, holding the HEAD
// the repo had before the first sync not yet activated. Entries are created
// with link(2), which fails if the name exists, so re-queueing a queued repo
// is a no-op that keeps the older rev - three quick commits, one build.

// QueuedActivate is one repo waiting for its ./activate.
type QueuedActivate struct {
	Rel    string
	OldRev string
	At     time.Time
}

func activateQueueDir() string { return filepath.Join(config.ActivateDir(), "queue") }

// queueName escapes rel into one flat file name. A leading dot is escaped
// too: dot-files in the queue directory are in-flight temp files.
func queueName(rel string) string {
	name := url.PathEscape(rel)
	if strings.HasPrefix(name, ".") {
		name = "%2E" + name[1:]
	}
	return name
}

// EnqueueActivate records that rel needs activating, covering every commit
// after oldRev. A no-op if rel is already queued.
func EnqueueActivate(rel, oldRev string) error {
	dir := activateQueueDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".new-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(oldRev + "\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	err = os.Link(tmp.Name(), filepath.Join(dir, queueName(rel)))
	if os.IsExist(err) {
		return nil
	}
	return err
}

// ActivateQueue lists the queued repos, oldest first.
func ActivateQueue() []QueuedActivate {
	dir := activateQueueDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var q []QueuedActivate
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		rel, err := url.PathUnescape(e.Name())
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		q = append(q, QueuedActivate{Rel: rel, OldRev: strings.TrimSpace(string(b)), At: info.ModTime()})
	}
	sort.SliceStable(q, func(i, j int) bool {
		if !q[i].At.Equal(q[j].At) {
			return q[i].At.Before(q[j].At)
		}
		return q[i].Rel < q[j].Rel
	})
	return q
}

// HasActivateQueue reports whether any repo is waiting for its ./activate.
func HasActivateQueue() bool { return len(ActivateQueue()) > 0 }

// takeOldestActivate removes and returns the oldest entry. Removing before
// running is what makes a sync that lands mid-run queue the repo again.
func takeOldestActivate() (QueuedActivate, bool) {
	for _, q := range ActivateQueue() {
		if err := os.Remove(filepath.Join(activateQueueDir(), queueName(q.Rel))); err == nil {
			return q, true
		}
	}
	return QueuedActivate{}, false
}

// dropQueuedActivate forgets rel's entry, if any.
func dropQueuedActivate(rel string) {
	_ = os.Remove(filepath.Join(activateQueueDir(), queueName(rel)))
}
