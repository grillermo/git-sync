package syncer

import (
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
)

// A machine that is offline when a commit is made misses it. Nothing about
// later commits in other repos carries it, and nothing about the machine
// coming back online announces itself. So a delivery that failed only because
// a machine was out of reach is remembered here and redone by the next sync
// run of any repo on this machine (see retryPending), until it gets through.
//
// Each entry is an empty marker file:
//
//	pending/push/<rel>           pushing rel failed because the remote was unreachable
//	pending/notify/<host>/<rel>  rel was pushed, but peer <host> could not be told
//
// Creating and removing a file are each atomic, so concurrent push and retry
// processes need no lock around this. The worst a race costs is a duplicate
// notification, and receive is idempotent.
const (
	pendingPush   = "push"
	pendingNotify = "notify"
)

func pendingPath(kind string, rest ...string) string {
	parts := []string{config.PendingDir(), kind}
	for _, r := range rest {
		// rel contains slashes. Escaping keeps each entry one flat file and
		// round-trips exactly.
		parts = append(parts, url.PathEscape(r))
	}
	return filepath.Join(parts...)
}

// markPending records a missed delivery. kind is pendingPush with the rel, or
// pendingNotify with the host and then the rel.
func markPending(kind string, rest ...string) {
	p := pendingPath(kind, rest...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	// No O_TRUNC: re-queueing an existing entry must keep its mtime, which
	// is when the delivery was first missed (git-sync status shows it).
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
}

func clearPending(kind string, rest ...string) {
	_ = os.Remove(pendingPath(kind, rest...))
}

// listPending returns the rels recorded under kind (and a host, for notify).
func listPending(kind string, rest ...string) []string {
	entries, err := os.ReadDir(pendingPath(kind, rest...))
	if err != nil {
		return nil
	}
	var rels []string
	for _, e := range entries {
		if rel, err := url.PathUnescape(e.Name()); err == nil && !e.IsDir() {
			rels = append(rels, rel)
		}
	}
	return rels
}

// HasPending reports whether any delivery is waiting to be retried.
func HasPending() bool {
	if len(listPending(pendingPush)) > 0 {
		return true
	}
	hosts, _ := os.ReadDir(pendingPath(pendingNotify))
	for _, h := range hosts {
		if h.IsDir() {
			if entries, _ := os.ReadDir(filepath.Join(pendingPath(pendingNotify), h.Name())); len(entries) > 0 {
				return true
			}
		}
	}
	return false
}

// Retry redoes every missed delivery. It is the `retry` subcommand, started
// in the background by a receive: a machine that is receiving is evidently
// back online, so whatever it missed sending while it was away can go now.
// Returns a process exit code.
func Retry() int {
	cfg, err := config.Load()
	if err != nil {
		return 1
	}
	retryPending(cfg, "", nil)
	return 0
}

// retryPending redoes the missed deliveries, skipping current (which the
// calling push just attempted). reached is what that push learned about each
// peer: a peer it just failed to reach is not tried again now.
func retryPending(cfg config.Config, current string, reached map[string]bool) {
	if reached == nil {
		reached = map[string]bool{}
	}

	// Pushes first: pushing a repo notifies every peer about it, which also
	// clears any notify entries for it.
	for _, rel := range listPending(pendingPush) {
		if rel == current {
			continue
		}
		if !retryable(cfg, rel) {
			clearPending(pendingPush, rel)
			continue
		}
		// One failure is enough to count a peer as down for this run.
		for host, ok := range pushRepo(cfg, rel) {
			if _, seen := reached[host]; !ok || !seen {
				reached[host] = ok
			}
		}
	}

	// Then each peer's backlog, peers in parallel as in notifyAll. A peer is
	// drained one repo at a time and abandoned at its first failure: if it
	// is down, every further attempt would just wait out ConnectTimeout.
	var wg sync.WaitGroup
	for _, p := range cfg.PeerList() {
		if ok, tried := reached[p.Host]; tried && !ok {
			continue
		}
		wg.Add(1)
		go func(p config.Peer) {
			defer wg.Done()
			for _, rel := range listPending(pendingNotify, p.Host) {
				if !retryable(cfg, rel) {
					clearPending(pendingNotify, p.Host, rel)
					continue
				}
				branch, _ := gitcmd.CurrentBranch(cfg.RepoPath(rel))
				if !notifyPeer(cfg, p, rel, branch) {
					return
				}
			}
		}(p)
	}
	wg.Wait()
}

// retryable is whether a remembered rel is still something to sync: the
// selection may have changed since it was recorded.
func retryable(cfg config.Config, rel string) bool {
	return cfg.ValidateRel(rel) == nil && cfg.IsSelected(rel) && !strings.Contains(rel, "'")
}

// PendingEntry is one missed delivery, for git-sync status. Kind is "push"
// or "notify"; Host is set for notify only. Since is when it was first
// queued.
type PendingEntry struct {
	Kind  string
	Rel   string
	Host  string
	Since time.Time
}

// ListAllPending returns every queued delivery, oldest first.
func ListAllPending() []PendingEntry {
	var out []PendingEntry
	add := func(kind, host, dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
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
			out = append(out, PendingEntry{Kind: kind, Rel: rel, Host: host, Since: info.ModTime()})
		}
	}
	add(pendingPush, "", pendingPath(pendingPush))
	hosts, _ := os.ReadDir(pendingPath(pendingNotify))
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		host, err := url.PathUnescape(h.Name())
		if err != nil {
			continue
		}
		add(pendingNotify, host, filepath.Join(pendingPath(pendingNotify), h.Name()))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}
