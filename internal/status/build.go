// Package status condenses this machine's activity log, pending queue and
// in-flight operations into one snapshot of every selected repo, for the
// hidden `git-sync status` command and the git-sync-status menu bar app
// that reads it. Build is pure; follow.go holds the I/O.
package status

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/syncer"
)

// State is a repo's one-word summary, highest-ranked first.
type State string

const (
	StateSyncing State = "syncing"
	StateError   State = "error"
	StateWarn    State = "warn"
	StateOK      State = "ok"
	StateNever   State = "never"
)

// Snapshot is one line of `git-sync status --json`. Every list is non-nil,
// so it marshals as [] rather than null, and every time is whole seconds,
// so Swift's .iso8601 date decoding accepts it.
type Snapshot struct {
	At       time.Time `json:"at"`
	Syncing  bool      `json:"syncing"`
	Problems int       `json:"problems"` // repos with at least one problem
	Repos    []Repo    `json:"repos"`
	Pending  []Pending `json:"pending"`
	Error    string    `json:"error,omitempty"`
}

type Repo struct {
	Repo     string     `json:"repo"`
	Path     string     `json:"path"`
	State    State      `json:"state"`
	LastSync *time.Time `json:"last_sync,omitempty"`
	Running  []Running  `json:"running"`
	Problems []Problem  `json:"problems"` // newest first
}

type Running struct {
	Op      activity.Op `json:"op"`
	Peer    string      `json:"peer,omitempty"`
	Started time.Time   `json:"started"`
}

type Problem struct {
	Time   time.Time       `json:"ts"`
	Op     activity.Op     `json:"op"`
	Peer   string          `json:"peer,omitempty"`
	Status activity.Status `json:"status"`
	Msg    string          `json:"msg"`
}

type Pending struct {
	Kind  string    `json:"kind"` // "push" or "notify"
	Repo  string    `json:"repo"`
	Path  string    `json:"path"`
	Peer  string    `json:"peer,omitempty"`
	Since time.Time `json:"since"`
}

// Input is everything Build reads. Events are oldest first, as
// activity.Read returns them.
type Input struct {
	Config  config.Config
	Events  []activity.Event
	Running []running.Marker
	Pending []syncer.PendingEntry
	Now     time.Time
}

// channel is one independent line of work for a repo: its pushes, its
// notifies to one peer, its receives, its ./activate runs. A problem on one
// channel is cleared only by a later success on that same channel - a fine
// push says nothing about a peer whose receive failed.
type channel struct {
	repo string
	op   activity.Op
	peer string
}

// Build turns the inputs into a snapshot. For each channel only its latest
// run counts: the channel has a problem if that run logged a warn or an
// error. Skips never count, and neither does offline: an out-of-reach
// machine is reported through the pending queue instead.
func Build(in Input) Snapshot {
	snap := Snapshot{At: sec(in.Now), Repos: []Repo{}, Pending: []Pending{}}

	byChan := map[channel][]int{}
	lastSync := map[string]time.Time{}
	lastSeen := map[string]time.Time{}
	for i, e := range in.Events {
		if !in.Config.IsSelected(e.Repo) {
			continue
		}
		if e.Time.After(lastSeen[e.Repo]) {
			lastSeen[e.Repo] = e.Time
		}
		switch effective(e) {
		case activity.StatusSkip, activity.StatusOffline:
			continue
		case activity.StatusOK:
			if e.Time.After(lastSync[e.Repo]) {
				lastSync[e.Repo] = e.Time
			}
		}
		k := channel{e.Repo, e.Op, e.Peer}
		byChan[k] = append(byChan[k], i)
	}

	problems := map[string][]Problem{}
	for k, idx := range byChan {
		last := idx[len(idx)-1] // the log is append-only: file order is time order
		run := runOf(in.Events[last], last)
		for _, i := range idx {
			e := in.Events[i]
			if runOf(e, i) != run || !e.IsProblem() {
				continue
			}
			problems[k.repo] = append(problems[k.repo], Problem{
				Time: sec(e.Time), Op: e.Op, Peer: e.Peer, Status: e.Status, Msg: e.Msg,
			})
		}
	}

	for _, rel := range in.Config.Repos {
		r := Repo{Repo: rel, Path: in.Config.RepoPath(rel), Running: []Running{}, Problems: []Problem{}}
		r.Problems = append(r.Problems, problems[rel]...)
		sort.SliceStable(r.Problems, func(i, j int) bool { return r.Problems[i].Time.After(r.Problems[j].Time) })
		if t, ok := lastSync[rel]; ok {
			t = sec(t)
			r.LastSync = &t
		}
		for _, m := range in.Running {
			if m.Repo == rel {
				r.Running = append(r.Running, Running{Op: m.Op, Peer: m.Peer, Started: sec(m.Started)})
			}
		}
		r.State = stateOf(r)
		if len(r.Running) > 0 {
			snap.Syncing = true
		}
		if len(r.Problems) > 0 {
			snap.Problems++
		}
		snap.Repos = append(snap.Repos, r)
	}
	sort.SliceStable(snap.Repos, func(i, j int) bool {
		a, b := snap.Repos[i], snap.Repos[j]
		if rank(a.State) != rank(b.State) {
			return rank(a.State) < rank(b.State)
		}
		if ta, tb := lastSeen[a.Repo], lastSeen[b.Repo]; !ta.Equal(tb) {
			return ta.After(tb)
		}
		return a.Repo < b.Repo
	})

	for _, p := range in.Pending {
		if !in.Config.IsSelected(p.Rel) {
			continue
		}
		snap.Pending = append(snap.Pending, Pending{
			Kind: p.Kind, Repo: p.Rel, Path: in.Config.RepoPath(p.Rel), Peer: p.Host, Since: sec(p.Since),
		})
	}
	sort.SliceStable(snap.Pending, func(i, j int) bool {
		if !snap.Pending[i].Since.Equal(snap.Pending[j].Since) {
			return snap.Pending[i].Since.Before(snap.Pending[j].Since)
		}
		return snap.Pending[i].Repo < snap.Pending[j].Repo
	})
	return snap
}

// effective reads a log line written before StatusOffline existed: those
// logged an out-of-reach delivery as an error whose message promised a
// retry. Delete once such logs no longer matter.
func effective(e activity.Event) activity.Status {
	if e.Status == activity.StatusError && strings.Contains(e.Msg, "will retry") {
		return activity.StatusOffline
	}
	return e.Status
}

// runOf is the event's run id, or a unique one for an event that is a run
// of its own.
func runOf(e activity.Event, i int) string {
	if e.Run != "" {
		return e.Run
	}
	return "#" + strconv.Itoa(i)
}

func stateOf(r Repo) State {
	switch {
	case len(r.Running) > 0:
		return StateSyncing
	case hasStatus(r.Problems, activity.StatusError):
		return StateError
	case len(r.Problems) > 0:
		return StateWarn
	case r.LastSync != nil:
		return StateOK
	}
	return StateNever
}

func hasStatus(ps []Problem, s activity.Status) bool {
	for _, p := range ps {
		if p.Status == s {
			return true
		}
	}
	return false
}

func rank(s State) int {
	switch s {
	case StateSyncing:
		return 0
	case StateError:
		return 1
	case StateWarn:
		return 2
	}
	return 3
}

func sec(t time.Time) time.Time { return t.Truncate(time.Second) }
