// Package report turns the activity log into something a human can read,
// either as a static text report or an interactive TUI.
package report

import (
	"sort"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
)

// Options narrows which events a report covers.
type Options struct {
	Since        time.Time // zero means no lower bound
	Repo         string    // substring match; empty means all repos
	ProblemsOnly bool      // only warnings and errors
}

// Summary is one repo's activity, which is how the report is grouped.
type Summary struct {
	Repo         string
	Total        int
	Problems     int
	Pushes       int
	Receives     int
	LastActivity time.Time
	LastProblem  time.Time
	LastMsg      string
	// Events for this repo, newest first.
	Events []activity.Event
}

// Totals aggregates across every repo, for the report's header line.
type Totals struct {
	Repos    int
	Events   int
	Problems int
}

// Filter keeps only the events every set option admits.
func Filter(events []activity.Event, o Options) []activity.Event {
	var out []activity.Event
	for _, e := range events {
		if !o.Since.IsZero() && e.Time.Before(o.Since) {
			continue
		}
		if o.Repo != "" && !strings.Contains(e.Repo, o.Repo) {
			continue
		}
		if o.ProblemsOnly && !e.IsProblem() {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Summarize groups events by repo, most recently active repo first - which is
// almost always the one you opened the report to look at.
func Summarize(events []activity.Event) []Summary {
	byRepo := map[string]*Summary{}
	for _, e := range events {
		s, ok := byRepo[e.Repo]
		if !ok {
			s = &Summary{Repo: e.Repo}
			byRepo[e.Repo] = s
		}
		s.Total++
		switch e.Op {
		case activity.OpPush:
			s.Pushes++
		case activity.OpReceive:
			s.Receives++
		}
		if e.IsProblem() {
			s.Problems++
			if e.Time.After(s.LastProblem) {
				s.LastProblem = e.Time
			}
		}
		if e.Time.After(s.LastActivity) {
			s.LastActivity = e.Time
			s.LastMsg = e.Msg
		}
		s.Events = append(s.Events, e)
	}

	out := make([]Summary, 0, len(byRepo))
	for _, s := range byRepo {
		// Newest first within a repo: the timeline reads top-down as history.
		sort.SliceStable(s.Events, func(i, j int) bool {
			return s.Events[i].Time.After(s.Events[j].Time)
		})
		out = append(out, *s)
	}
	// Ranging a map yields repos in random order, so ties must break on the
	// name or the report reorders itself between runs.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].Repo < out[j].Repo
		}
		return out[i].LastActivity.After(out[j].LastActivity)
	})
	return out
}

// Totalize aggregates across repos. Named Totalize, not Totals: Go has one
// namespace for types and functions, so `func Totals` would collide with the
// `Totals` type above.
func Totalize(summaries []Summary) Totals {
	t := Totals{Repos: len(summaries)}
	for _, s := range summaries {
		t.Events += s.Total
		t.Problems += s.Problems
	}
	return t
}
