package report_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/report"
)

// Msg is derived from repo and minute so every event carries a distinguishable
// message: a test can then say *which* event a summary's LastMsg came from.
func ev(repo string, op activity.Op, st activity.Status, min int) activity.Event {
	return activity.Event{
		Time: time.Date(2026, 8, 22, 12, min, 0, 0, time.UTC),
		Repo: repo, Op: op, Status: st,
		Msg: fmt.Sprintf("%s@%d", repo, min),
	}
}

func TestSummarizeGroupsByRepo(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("b/two", activity.OpPush, activity.StatusOK, 2),
		ev("a/one", activity.OpReceive, activity.StatusOK, 3),
	}
	got := report.Summarize(events)
	if len(got) != 2 {
		t.Fatalf("got %d repos, want 2", len(got))
	}
}

func TestSummarizeCountsByOutcome(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("a/one", activity.OpPush, activity.StatusError, 2),
		ev("a/one", activity.OpReceive, activity.StatusWarn, 3),
		ev("a/one", activity.OpNotify, activity.StatusSkip, 4),
	}
	s := report.Summarize(events)[0]
	if s.Total != 4 {
		t.Errorf("Total = %d, want 4", s.Total)
	}
	if s.Problems != 2 {
		t.Errorf("Problems = %d, want 2 (one error + one warn)", s.Problems)
	}
	if s.Pushes != 2 {
		t.Errorf("Pushes = %d, want 2", s.Pushes)
	}
	if s.Receives != 1 {
		t.Errorf("Receives = %d, want 1", s.Receives)
	}
}

func TestSummarizeOrdersByMostRecentActivity(t *testing.T) {
	events := []activity.Event{
		ev("old/repo", activity.OpPush, activity.StatusOK, 1),
		ev("new/repo", activity.OpPush, activity.StatusOK, 9),
		ev("mid/repo", activity.OpPush, activity.StatusOK, 5),
	}
	got := report.Summarize(events)
	want := []string{"new/repo", "mid/repo", "old/repo"}
	for i, w := range want {
		if got[i].Repo != w {
			t.Errorf("position %d = %q, want %q", i, got[i].Repo, w)
		}
	}
}

// Repos grouped from a map come out in random order, so a tie on LastActivity
// has to break on something deterministic or the report shuffles run to run.
func TestSummarizeBreaksOrderTiesByRepoName(t *testing.T) {
	events := []activity.Event{
		ev("c/three", activity.OpPush, activity.StatusOK, 4),
		ev("a/one", activity.OpPush, activity.StatusOK, 4),
		ev("b/two", activity.OpPush, activity.StatusOK, 4),
	}
	want := []string{"a/one", "b/two", "c/three"}
	for range 20 { // map order varies per iteration; one pass could pass by luck
		got := report.Summarize(events)
		for i, w := range want {
			if got[i].Repo != w {
				t.Fatalf("position %d = %q, want %q", i, got[i].Repo, w)
			}
		}
	}
}

func TestSummarizeTracksLastActivityAndLastProblem(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusError, 1),
		ev("a/one", activity.OpPush, activity.StatusOK, 7),
	}
	s := report.Summarize(events)[0]
	if s.LastActivity.Minute() != 7 {
		t.Errorf("LastActivity = %v, want 12:07", s.LastActivity)
	}
	if s.LastProblem.Minute() != 1 {
		t.Errorf("LastProblem = %v, want 12:01", s.LastProblem)
	}
}

// The report prints LastMsg beside the repo, so it has to be the newest
// event's message - not the first seen, and not whatever happened to be last
// in the input. The newest event sits in the middle here to catch both.
func TestSummarizeTracksLastMsgFromNewestEvent(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("a/one", activity.OpPush, activity.StatusOK, 9),
		ev("a/one", activity.OpPush, activity.StatusOK, 5),
	}
	s := report.Summarize(events)[0]
	if s.LastMsg != "a/one@9" {
		t.Errorf("LastMsg = %q, want the 12:09 message %q", s.LastMsg, "a/one@9")
	}
}

// Three events in shuffled order, not two: with two, merely reversing the input
// would pass and prove nothing about sorting.
func TestSummarizeKeepsEventsNewestFirst(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("a/one", activity.OpPush, activity.StatusOK, 9),
		ev("a/one", activity.OpPush, activity.StatusOK, 5),
	}
	s := report.Summarize(events)[0]
	if len(s.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(s.Events))
	}
	want := []int{9, 5, 1}
	for i, w := range want {
		if got := s.Events[i].Time.Minute(); got != w {
			t.Errorf("Events[%d] = 12:%02d, want 12:%02d: history should read newest first", i, got, w)
		}
	}
}

// Summary is a value, and callers copy values freely - a bubbletea Update
// returns a copied model on every keystroke. If Events came out carrying spare
// capacity, two copies appending would each write index len(Events) of the same
// backing array and the second would silently clobber the first. That is the
// aliasing bug 7f8146a fixed in the picker; this pins it shut here.
func TestSummarizeEventsDoNotAliasAcrossSummaryCopies(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("a/one", activity.OpPush, activity.StatusOK, 9),
		ev("a/one", activity.OpPush, activity.StatusOK, 5),
	}
	s := report.Summarize(events)[0]

	a, b := s, s
	a.Events = append(a.Events, ev("a/one", activity.OpPush, activity.StatusOK, 11))
	b.Events = append(b.Events, ev("a/one", activity.OpPush, activity.StatusOK, 22))

	if got := a.Events[len(a.Events)-1].Msg; got != "a/one@11" {
		t.Errorf("copy a's appended event = %q, want %q: the copies share a backing array", got, "a/one@11")
	}
	if got := b.Events[len(b.Events)-1].Msg; got != "a/one@22" {
		t.Errorf("copy b's appended event = %q, want %q", got, "a/one@22")
	}
}

func TestSummarizeEmptyInput(t *testing.T) {
	if got := report.Summarize(nil); len(got) != 0 {
		t.Errorf("got %d summaries, want 0", len(got))
	}
}

// The cutoff is inclusive: an event landing exactly on Since is kept, so
// `--since 12:15` twice in a row can't drop an event the first run showed.
func TestFilterSince(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("a/one", activity.OpPush, activity.StatusOK, 15), // exactly the cutoff
		ev("a/one", activity.OpPush, activity.StatusOK, 30),
	}
	cutoff := time.Date(2026, 8, 22, 12, 15, 0, 0, time.UTC)
	got := report.Filter(events, report.Options{Since: cutoff})
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (12:15 and 12:30)", len(got))
	}
	if got[0].Time.Minute() != 15 {
		t.Errorf("first kept event = %v, want the 12:15 event on the cutoff", got[0].Time)
	}
}

func TestFilterProblemsOnly(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("a/one", activity.OpPush, activity.StatusWarn, 2),
		ev("a/one", activity.OpPush, activity.StatusSkip, 3),
	}
	got := report.Filter(events, report.Options{ProblemsOnly: true})
	if len(got) != 1 || got[0].Status != activity.StatusWarn {
		t.Errorf("got %+v, want just the warn", got)
	}
}

func TestFilterByRepoSubstring(t *testing.T) {
	events := []activity.Event{
		ev("work/api", activity.OpPush, activity.StatusOK, 1),
		ev("personal/notes", activity.OpPush, activity.StatusOK, 2),
	}
	got := report.Filter(events, report.Options{Repo: "work"})
	if len(got) != 1 || got[0].Repo != "work/api" {
		t.Errorf("got %+v, want just work/api", got)
	}
}

func TestFilterZeroOptionsKeepsEverything(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("b/two", activity.OpNotify, activity.StatusSkip, 2),
	}
	if got := report.Filter(events, report.Options{}); len(got) != 2 {
		t.Errorf("got %d events, want all 2", len(got))
	}
}

// The three options narrow together, not one at a time.
func TestFilterCombinesOptions(t *testing.T) {
	events := []activity.Event{
		ev("work/api", activity.OpPush, activity.StatusError, 1),        // too old
		ev("personal/notes", activity.OpPush, activity.StatusError, 20), // wrong repo
		ev("work/api", activity.OpPush, activity.StatusOK, 25),          // not a problem
		ev("work/api", activity.OpPush, activity.StatusError, 30),       // keeper
	}
	got := report.Filter(events, report.Options{
		Since:        time.Date(2026, 8, 22, 12, 15, 0, 0, time.UTC),
		Repo:         "work",
		ProblemsOnly: true,
	})
	if len(got) != 1 || got[0].Time.Minute() != 30 {
		t.Errorf("got %+v, want just the 12:30 work/api error", got)
	}
}

func TestTotalizeAcrossAllRepos(t *testing.T) {
	events := []activity.Event{
		ev("a/one", activity.OpPush, activity.StatusOK, 1),
		ev("b/two", activity.OpPush, activity.StatusError, 2),
	}
	tot := report.Totalize(report.Summarize(events))
	if tot.Repos != 2 || tot.Events != 2 || tot.Problems != 1 {
		t.Errorf("Totalize = %+v", tot)
	}
}

func TestTotalizeEmptyInput(t *testing.T) {
	if tot := report.Totalize(nil); tot != (report.Totals{}) {
		t.Errorf("Totalize(nil) = %+v, want zero", tot)
	}
}
