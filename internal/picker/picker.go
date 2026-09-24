// Package picker adapts the repo scan to chicle, the checkbox list that
// chooses which repos to sync.
package picker

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/grillermo/chicle"

	"github.com/grillermo/git-sync/internal/scan"
)

const (
	sectionNew     = "NEW"
	sectionSyncing = "ALREADY SYNCING"
	sectionMissing = "MISSING (in your config, not found on disk)"
)

// savedPrefix marks a Save result. chicle returns "" for a quit, so a bare
// join of the ticked repos could not tell "saved nothing" from "cancelled".
const savedPrefix = "saved\n"

// Rows builds the list. Order is deliberate: new repos first, because on a
// re-run they are the only thing that changed.
func Rows(discovered []scan.Repo, selected []string) []chicle.Row {
	inConfig := map[string]bool{}
	for _, r := range selected {
		inConfig[r] = true
	}
	seen := map[string]bool{}

	var news, syncing, missing []chicle.Row
	for _, r := range discovered {
		seen[r.Rel] = true
		row := chicle.Row{Key: r.Rel, Cols: []string{r.Rel, describe(r)}}
		if inConfig[r.Rel] {
			// Already installed: stays ticked so a re-run only ever adds repos.
			// Removing one is deliberate - edit config.toml or uninstall.
			row.Section, row.Ticked, row.Locked = sectionSyncing, true, true
			syncing = append(syncing, row)
		} else {
			row.Section = sectionNew
			news = append(news, row)
		}
	}
	// Anything in the config the scan did not find: keep it, ticked, rather
	// than dropping a repo just because its volume was not mounted today.
	for _, rel := range selected {
		if !seen[rel] {
			missing = append(missing, chicle.Row{
				Key:     rel,
				Cols:    []string{rel, "not found on disk"},
				Section: sectionMissing,
				Ticked:  true,
			})
		}
	}

	// Headings are noise on a first install, where everything is new.
	if len(syncing)+len(missing) == 0 {
		for i := range news {
			news[i].Section = ""
		}
	}
	return append(append(news, syncing...), missing...)
}

// Config is the whole picker, ready for chicle.Run.
func Config(discovered []scan.Repo, selected []string) chicle.Config {
	return chicle.Config{
		Title:       "SELECT REPOS TO SYNC",
		Columns:     []chicle.Column{{Title: "REPO", Width: 32}, {Title: "REMOTE"}},
		Rows:        Rows(discovered, selected),
		MultiSelect: true,
		Actions: []chicle.Action{
			{Label: "Save", Run: func(s chicle.Selection) chicle.Outcome {
				rels := make([]string, len(s.Ticked))
				for i, r := range s.Ticked {
					rels[i] = r.Key
				}
				return chicle.Outcome{Result: encode(rels), Done: true}
			}},
			{Label: "Cancel"},
		},
	}
}

// Choose runs the picker on the terminal. ok is false when the user cancelled.
func Choose(discovered []scan.Repo, selected []string) (repos []string, ok bool, err error) {
	return choose(chicle.Run, discovered, selected)
}

func choose(run func(chicle.Config) (string, error), discovered []scan.Repo, selected []string) ([]string, bool, error) {
	if len(discovered)+len(selected) == 0 {
		return nil, false, errors.New(
			"No git repos found under that directory. Clone something under it, then run install again.")
	}
	res, err := run(Config(discovered, selected))
	if err != nil {
		return nil, false, err
	}
	repos, ok := decode(res)
	return repos, ok, nil
}

// encode sorts: the result is written straight to config.toml, so it must be
// deterministic.
func encode(rels []string) string {
	sorted := append([]string(nil), rels...)
	sort.Strings(sorted)
	return savedPrefix + strings.Join(sorted, "\n")
}

func decode(res string) ([]string, bool) {
	body, ok := strings.CutPrefix(res, savedPrefix)
	if !ok {
		return nil, false
	}
	if body == "" {
		return nil, true
	}
	return strings.Split(body, "\n"), true
}

// describe is the right-hand column. The remote leads it: syncing happens
// through the remote, so "which remote" is the first thing that decides
// whether ticking this repo will do anything.
func describe(r scan.Repo) string {
	if !r.CanSync() {
		return "no remote - cannot sync"
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
