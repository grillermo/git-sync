// Package picker is the checkbox list that chooses which repos to sync.
package picker

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/grillermo/git-sync/internal/scan"
)

type section int

const (
	sectionNew section = iota
	sectionSyncing
	sectionMissing
)

var sectionLabel = map[section]string{
	sectionNew:     "NEW",
	sectionSyncing: "ALREADY SYNCING",
	sectionMissing: "MISSING (in your config, not found on disk)",
}

type row struct {
	repo    scan.Repo
	section section
	ticked  bool
}

// Model is the picker. Exported, and free of terminal access, so tests can
// drive it directly.
type Model struct {
	rows      []row
	cursor    int
	confirmed bool
	cancelled bool
	width     int
	hasOld    bool // was anything already syncing? drives header visibility
}

// New builds the picker from what the scan found and what is already in the
// config. Order is deliberate: new repos first, because on a re-run they are
// the only thing that changed.
func New(discovered []scan.Repo, selected []string) Model {
	inConfig := map[string]bool{}
	for _, r := range selected {
		inConfig[r] = true
	}
	seen := map[string]bool{}

	var news, syncing []row
	for _, r := range discovered {
		seen[r.Rel] = true
		if inConfig[r.Rel] {
			syncing = append(syncing, row{repo: r, section: sectionSyncing, ticked: true})
		} else {
			news = append(news, row{repo: r, section: sectionNew, ticked: false})
		}
	}
	// Anything in the config the scan did not find: keep it, ticked, rather
	// than dropping a repo just because its volume was not mounted today.
	var missing []row
	for _, rel := range selected {
		if !seen[rel] {
			missing = append(missing, row{
				repo:    scan.Repo{Rel: rel},
				section: sectionMissing,
				ticked:  true,
			})
		}
	}

	m := Model{hasOld: len(syncing)+len(missing) > 0}
	m.rows = append(append(append(m.rows, news...), syncing...), missing...)
	return m
}

func (m Model) Init() tea.Cmd { return nil }

// Selected returns the ticked repo paths, sorted - the result is written
// straight to config.toml, so it must be deterministic.
func (m Model) Selected() []string {
	var out []string
	for _, r := range m.rows {
		if r.ticked {
			out = append(out, r.repo.Rel)
		}
	}
	sort.Strings(out)
	return out
}

func (m Model) Confirmed() bool { return m.confirmed }
func (m Model) Cancelled() bool { return m.cancelled }

// RelAt is the repo shown at row i, for tests and for ordering assertions.
func (m Model) RelAt(i int) string {
	if i < 0 || i >= len(m.rows) {
		return ""
	}
	return m.rows[i].repo.Rel
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case "enter":
			m.confirmed = true
			return m, tea.Quit
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case " ", "space", "x":
			if len(m.rows) > 0 {
				rows := append([]row(nil), m.rows...)
				rows[m.cursor].ticked = !rows[m.cursor].ticked
				m.rows = rows
			}
		case "a":
			rows := append([]row(nil), m.rows...)
			for i := range rows {
				rows[i].ticked = true
			}
			m.rows = rows
		case "n":
			rows := append([]row(nil), m.rows...)
			for i := range rows {
				rows[i].ticked = false
			}
			m.rows = rows
		}
	}
	return m, nil
}

func (m Model) View() string {
	title := titleStyle.Render("SELECT REPOS TO SYNC")
	if len(m.rows) == 0 {
		return title + "\n\n" + "No git repos found under that directory.\n" +
			dimStyle.Render("Clone something under it, then run install again.\n") +
			dimStyle.Render("[q] quit")
	}

	var b strings.Builder
	b.WriteString(title + "\n\n")

	current := section(-1)
	for i, r := range m.rows {
		// Headers are noise on a first install, where everything is new.
		if r.section != current {
			current = r.section
			if m.hasOld {
				if i > 0 {
					b.WriteString("\n")
				}
				b.WriteString("  " + dimStyle.Render(sectionLabel[current]) + "\n")
			}
		}

		box := "[ ]"
		if r.ticked {
			box = "[x]"
		}
		line := fmt.Sprintf("%s %-32s %s", box, r.repo.Rel, describe(r.repo, r.section))
		if i == m.cursor {
			b.WriteString(selectedStyle.Render("> "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\n" + dimStyle.Render(
		"[space] toggle   [a] all   [n] none   [enter] save   [q] cancel"))
	return b.String()
}

// describe is the right-hand metadata column. The remote leads it: syncing
// happens through the remote, so "which remote" is the first thing that
// decides whether ticking this repo will do anything.
//
// A MISSING row has no remote populated at all - not because it lacks one,
// but because it was not found on disk this scan, so it is special-cased
// before the remote-based logic below can produce a misleading message.
func describe(r scan.Repo, sec section) string {
	if sec == sectionMissing {
		return dimStyle.Render("not found on disk")
	}
	if !r.CanSync() {
		return warnStyle.Render("no remote - cannot sync")
	}
	s := r.Remote
	if r.Commits == 0 && r.LastCommit.IsZero() {
		return dimStyle.Render(s + "   no commits")
	}
	s += fmt.Sprintf("   %d commits", r.Commits)
	if !r.LastCommit.IsZero() {
		s += fmt.Sprintf("   last commit %s ago", roughly(time.Since(r.LastCommit)))
	}
	return dimStyle.Render(s)
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

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)
