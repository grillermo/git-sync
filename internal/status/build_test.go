package status_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/running"
	"github.com/grillermo/git-sync/internal/status"
	"github.com/grillermo/git-sync/internal/syncer"
)

var base = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

func cfg(repos ...string) config.Config { return config.Config{BaseDir: "/code", Repos: repos} }

func ev(min int, repo string, op activity.Op, st activity.Status, msg string) activity.Event {
	return activity.Event{Time: at(min), Repo: repo, Op: op, Status: st, Msg: msg}
}

func notify(min int, repo, peer string, st activity.Status, msg string) activity.Event {
	e := ev(min, repo, activity.OpNotify, st, msg)
	e.Peer = peer
	return e
}

func build(c config.Config, events ...activity.Event) status.Snapshot {
	return status.Build(status.Input{Config: c, Events: events, Now: at(60)})
}

func repo(t *testing.T, s status.Snapshot, rel string) status.Repo {
	t.Helper()
	for _, r := range s.Repos {
		if r.Repo == rel {
			return r
		}
	}
	t.Fatalf("no repo %q in %+v", rel, s.Repos)
	return status.Repo{}
}

func TestARepoWithNoActivityIsNever(t *testing.T) {
	r := repo(t, build(cfg("a")), "a")
	if r.State != status.StateNever || r.LastSync != nil || r.Path != "/code/a" {
		t.Errorf("got %+v", r)
	}
}

func TestALaterSuccessOnTheSameChannelClearsAProblem(t *testing.T) {
	s := build(cfg("a"),
		ev(1, "a", activity.OpPush, activity.StatusError, "rejected"),
		ev(2, "a", activity.OpPush, activity.StatusOK, "pushed"),
	)
	r := repo(t, s, "a")
	if r.State != status.StateOK || len(r.Problems) != 0 || s.Problems != 0 {
		t.Errorf("got %+v", r)
	}
	if r.LastSync == nil || !r.LastSync.Equal(at(2)) {
		t.Errorf("LastSync = %v, want %v", r.LastSync, at(2))
	}
}

func TestASuccessOnAnotherChannelDoesNotClearAProblem(t *testing.T) {
	s := build(cfg("a"),
		notify(1, "a", "mini", activity.StatusError, "peer mini receive failed (exit 1)"),
		ev(2, "a", activity.OpPush, activity.StatusOK, "pushed"),
		notify(2, "a", "laptop", activity.StatusOK, "peer laptop synced"),
	)
	r := repo(t, s, "a")
	if r.State != status.StateError || len(r.Problems) != 1 || r.Problems[0].Peer != "mini" {
		t.Errorf("got %+v", r)
	}
	if s.Problems != 1 {
		t.Errorf("Snapshot.Problems = %d, want 1", s.Problems)
	}
}

func TestOfflineIsNeverAProblem(t *testing.T) {
	s := build(cfg("a", "b"),
		notify(1, "a", "pc", activity.StatusOffline, "peer pc unreachable, will retry"),
		// A line written before the offline status existed.
		notify(1, "b", "pc", activity.StatusError, "peer pc unreachable, will retry"),
	)
	for _, rel := range []string{"a", "b"} {
		if r := repo(t, s, rel); len(r.Problems) != 0 {
			t.Errorf("%s: offline counted as a problem: %+v", rel, r)
		}
	}
}

func TestOfflineDoesNotClearAProblem(t *testing.T) {
	r := repo(t, build(cfg("a"),
		notify(1, "a", "pc", activity.StatusError, "receive failed"),
		notify(2, "a", "pc", activity.StatusOffline, "unreachable, will retry"),
	), "a")
	if r.State != status.StateError {
		t.Errorf("got %+v", r)
	}
}

func TestSkipsAreIgnored(t *testing.T) {
	r := repo(t, build(cfg("a"),
		ev(1, "a", activity.OpPush, activity.StatusError, "rejected"),
		ev(2, "a", activity.OpPush, activity.StatusSkip, "detached HEAD"),
	), "a")
	if r.State != status.StateError {
		t.Errorf("got %+v", r)
	}
}

func receiveRun(min int, run string, st activity.Status, msg string) activity.Event {
	e := ev(min, "a", activity.OpReceive, st, msg)
	e.Run = run
	return e
}

func TestAWarningInsideTheLatestReceiveRunCounts(t *testing.T) {
	r := repo(t, build(cfg("a"),
		receiveRun(1, "r1", activity.StatusOK, "stashed dirty working tree"),
		receiveRun(1, "r1", activity.StatusWarn, "diverged, manual merge needed"),
		receiveRun(1, "r1", activity.StatusOK, "restored stashed changes"),
	), "a")
	if r.State != status.StateWarn || len(r.Problems) != 1 {
		t.Errorf("got %+v", r)
	}
}

func TestANewReceiveRunClearsTheLastRunsWarning(t *testing.T) {
	r := repo(t, build(cfg("a"),
		receiveRun(1, "r1", activity.StatusWarn, "diverged"),
		receiveRun(5, "r2", activity.StatusOK, "fast-forwarded main from origin"),
	), "a")
	if r.State != status.StateOK || len(r.Problems) != 0 {
		t.Errorf("got %+v", r)
	}
}

func TestRunningMakesARepoSyncingAndKeepsItsProblems(t *testing.T) {
	s := status.Build(status.Input{
		Config:  cfg("a"),
		Events:  []activity.Event{ev(1, "a", activity.OpPush, activity.StatusError, "rejected")},
		Running: []running.Marker{{Repo: "a", Op: activity.OpPush, Started: at(59)}},
		Now:     at(60),
	})
	r := repo(t, s, "a")
	if !s.Syncing || r.State != status.StateSyncing || len(r.Running) != 1 || len(r.Problems) != 1 {
		t.Errorf("got syncing=%v %+v", s.Syncing, r)
	}
}

func TestReposAreOrderedSyncingErrorWarnThenRecent(t *testing.T) {
	s := status.Build(status.Input{
		Config: cfg("old", "recent", "warned", "failed", "busy"),
		Events: []activity.Event{
			ev(1, "old", activity.OpPush, activity.StatusOK, ""),
			ev(9, "recent", activity.OpPush, activity.StatusOK, ""),
			ev(2, "warned", activity.OpPush, activity.StatusWarn, ""),
			ev(3, "failed", activity.OpPush, activity.StatusError, ""),
		},
		Running: []running.Marker{{Repo: "busy", Op: activity.OpReceive, Started: at(59)}},
		Now:     at(60),
	})
	var got []string
	for _, r := range s.Repos {
		got = append(got, r.Repo)
	}
	want := "busy failed warned recent old"
	if strings.Join(got, " ") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

func TestUnselectedReposAreLeftOut(t *testing.T) {
	s := status.Build(status.Input{
		Config:  cfg("a"),
		Events:  []activity.Event{ev(1, "gone", activity.OpPush, activity.StatusError, "x")},
		Running: []running.Marker{{Repo: "gone", Op: activity.OpPush, Started: at(1)}},
		Pending: []syncer.PendingEntry{{Kind: "push", Rel: "gone", Since: at(1)}},
		Now:     at(60),
	})
	if len(s.Repos) != 1 || len(s.Pending) != 0 || s.Syncing || s.Problems != 0 {
		t.Errorf("got %+v", s)
	}
}

func TestPendingIsListedOldestFirstWithPaths(t *testing.T) {
	s := status.Build(status.Input{
		Config: cfg("a", "b"),
		Pending: []syncer.PendingEntry{
			{Kind: "notify", Rel: "b", Host: "pc", Since: at(5)},
			{Kind: "push", Rel: "a", Since: at(2)},
		},
		Now: at(60),
	})
	if len(s.Pending) != 2 || s.Pending[0].Repo != "a" || s.Pending[1].Peer != "pc" || s.Pending[1].Path != "/code/b" {
		t.Errorf("got %+v", s.Pending)
	}
}

func TestSnapshotJSONHasNoNullLists(t *testing.T) {
	b, err := json.Marshal(build(cfg("a")))
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{`"repos":[`, `"pending":[]`, `"running":[]`, `"problems":[]`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON lacks %s: %s", want, js)
		}
	}
	if strings.Contains(js, "null") {
		t.Errorf("JSON has a null: %s", js)
	}
}

func TestTimesAreWholeSeconds(t *testing.T) {
	e := ev(1, "a", activity.OpPush, activity.StatusOK, "")
	e.Time = e.Time.Add(123 * time.Millisecond)
	r := repo(t, build(cfg("a"), e), "a")
	if r.LastSync.Nanosecond() != 0 {
		t.Errorf("LastSync %v keeps fractional seconds; the Swift decoder rejects them", r.LastSync)
	}
}
