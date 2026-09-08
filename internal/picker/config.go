// Package picker builds the chicle.Config for the repo picker. The picker's
// own state, filtering, and rendering all now live in chicle; this package's
// only job is turning a scan result and the current config into rows chicle
// can show, and turning a chicle.Selection back into a sorted repo list.
package picker

import (
	"fmt"
	"sort"
	"time"

	"github.com/grillermo/chicle"

	"github.com/grillermo/git-sync/internal/scan"
)

const (
	sectionNew     = "NEW"
	sectionSyncing = "ALREADY SYNCING"
	sectionMissing = "MISSING (in your config, not found on disk)"
)

// Config builds the chicle.Config for the repo picker. Order is deliberate:
// new repos first, because on a re-run they are the only thing that changed.
func Config(discovered []scan.Repo, selected []string) chicle.Config {
	inConfig := map[string]bool{}
	for _, r := range selected {
		inConfig[r] = true
	}
	seen := map[string]bool{}

	var rows []chicle.Row
	var syncingOrMissing []chicle.Row
	for _, r := range discovered {
		seen[r.Rel] = true
		if inConfig[r.Rel] {
			syncingOrMissing = append(syncingOrMissing, chicle.Row{
				Key: r.Rel, Section: sectionSyncing, Locked: true,
				Cols: []string{r.Rel, describe(r, false)},
			})
		} else {
			rows = append(rows, chicle.Row{
				Key: r.Rel, Section: sectionNew,
				Cols: []string{r.Rel, describe(r, false)},
			})
		}
	}
	// Anything in the config the scan did not find: keep it, ticked, rather
	// than dropping a repo just because its volume was not mounted today.
	for _, rel := range selected {
		if !seen[rel] {
			syncingOrMissing = append(syncingOrMissing, chicle.Row{
				Key: rel, Section: sectionMissing, Locked: true,
				Cols: []string{rel, describe(scan.Repo{Rel: rel}, true)},
			})
		}
	}
	rows = append(rows, syncingOrMissing...)

	return chicle.Config{
		Title:       "SELECT REPOS TO SYNC",
		Columns:     []chicle.Column{{Title: "REPO", Width: 32}, {Title: ""}},
		Rows:        rows,
		MultiSelect: true,
	}
}

// Selected sorts the ticked repo paths — the result is written straight to
// config.toml, so it must be deterministic regardless of row/display order.
func Selected(sel chicle.Selection) []string {
	out := make([]string, len(sel.Ticked))
	for i, r := range sel.Ticked {
		out[i] = r.Key
	}
	sort.Strings(out)
	return out
}

// describe is the right-hand metadata column, ported unchanged from the old
// picker.go's describe/roughly.
func describe(r scan.Repo, missing bool) string {
	if missing {
		return "not found on disk"
	}
	if !r.CanSync() {
		return "⚠ no remote - cannot sync"
	}
	s := r.Remote
	if r.Commits == 0 && r.LastCommit.IsZero() {
		return s + "   no commits"
	}
	s += fmt.Sprintf("   %d commits", r.Commits)
	if !r.LastCommit.IsZero() {
		s += fmt.Sprintf("   last commit %s ago", roughly(time.Since(r.LastCommit)))
	}
	return s
}

func roughly(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
