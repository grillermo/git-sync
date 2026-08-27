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
	// locked marks a repo that is already installed (in the config): it stays
	// ticked and cannot be unticked here, so a re-run only ever adds repos.
	// Removing one is a deliberate act - edit config.toml or uninstall.
	locked bool
}

// Model is the picker. Exported, and free of terminal access, so tests can
// drive it directly.
type Model struct {
	rows      []row
	cursor    int // index into the visible (filtered) rows, not into rows
	confirmed bool
	cancelled bool
	width     int
	height    int
	hasOld    bool // was anything already syncing? drives header visibility
	// filtering is the live "/" search mode: keystrokes edit filter instead
	// of driving the list. filter narrows the visible rows by substring on
	// the repo path; it never touches which rows are ticked, so Selected()
	// still sees everything and a filter can't drop a repo from the config.
	filtering bool
	filter    string
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
			syncing = append(syncing, row{repo: r, section: sectionSyncing, ticked: true, locked: true})
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

// visible is the row indices the filter lets through, in display order. An
// empty filter shows everything. Matching is a case-insensitive substring on
// the repo path.
func (m Model) visible() []int {
	if m.filter == "" {
		idx := make([]int, len(m.rows))
		for i := range m.rows {
			idx[i] = i
		}
		return idx
	}
	q := strings.ToLower(m.filter)
	var idx []int
	for i, r := range m.rows {
		if strings.Contains(strings.ToLower(r.repo.Rel), q) {
			idx = append(idx, i)
		}
	}
	return idx
}

// realIndex maps the cursor (a position in the visible list) back to a row
// index, or -1 when nothing is visible.
func (m Model) realIndex() int {
	vis := m.visible()
	if len(vis) == 0 {
		return -1
	}
	if m.cursor >= len(vis) {
		return vis[len(vis)-1]
	}
	return vis[m.cursor]
}

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
		m.height = msg.Height

	case tea.KeyMsg:
		if m.filtering {
			return m.updateFiltering(msg)
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case "/":
			m.filtering = true
			return m, nil
		case "enter":
			m.confirmed = true
			return m, tea.Quit
		case "down", "j":
			if m.cursor < len(m.visible())-1 {
				m.cursor++
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case " ", "space", "x":
			m = m.toggleCursor()
		case "a":
			rows := append([]row(nil), m.rows...)
			for i := range rows {
				rows[i].ticked = true
			}
			m.rows = rows
		case "n":
			rows := append([]row(nil), m.rows...)
			for i := range rows {
				// Locked rows can never be unticked, not even by "none".
				rows[i].ticked = rows[i].locked
			}
			m.rows = rows
		}
	}
	return m, nil
}

// updateFiltering handles keystrokes while the "/" search is open. Printable
// runes edit the query; esc leaves search and clears it, ctrl+w wipes the
// query but stays in search. The list is still navigable (arrows) and
// toggleable (space) so you can tick filtered results in place.
func (m Model) updateFiltering(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.cancelled = true
		return m, tea.Quit
	case "esc":
		m.filtering = false
		m.filter = ""
		m.cursor = 0
		return m, nil
	case "ctrl+w":
		m.filter = ""
		m.cursor = 0
		return m, nil
	case "enter":
		m.confirmed = true
		return m, tea.Quit
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
			m.cursor = 0
		}
		return m, nil
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down":
		if m.cursor < len(m.visible())-1 {
			m.cursor++
		}
		return m, nil
	case " ", "space":
		return m.toggleCursor(), nil
	}
	if msg.Type == tea.KeyRunes {
		m.filter += string(msg.Runes)
		m.cursor = 0
	}
	return m, nil
}

// toggleCursor flips the ticked state of the row under the cursor, copying the
// backing array first so saved snapshots stay independent. A locked row is
// already installed, so toggling it is a no-op.
func (m Model) toggleCursor() Model {
	ri := m.realIndex()
	if ri < 0 || m.rows[ri].locked {
		return m
	}
	rows := append([]row(nil), m.rows...)
	rows[ri].ticked = !rows[ri].ticked
	m.rows = rows
	return m
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
	if m.filtering {
		b.WriteString("  " + selectedStyle.Render("/"+m.filter) + dimStyle.Render(" (esc to clear)") + "\n\n")
	}

	lines, at := m.body()
	if len(lines) == 0 {
		b.WriteString("  " + dimStyle.Render("no repos match") + "\n")
		b.WriteString("\n" + dimStyle.Render(m.hint()))
		return b.String()
	}

	cursorLine := 0
	if m.cursor < len(at) {
		cursorLine = at[m.cursor]
	}
	start, end := m.window(len(lines), cursorLine)

	if start > 0 {
		b.WriteString("  " + dimStyle.Render(fmt.Sprintf("%d more above", countRows(at, 0, start))) + "\n")
	}
	for _, line := range lines[start:end] {
		b.WriteString(line + "\n")
	}
	if end < len(lines) {
		b.WriteString("  " + dimStyle.Render(fmt.Sprintf("%d more below", countRows(at, end, len(lines)))) + "\n")
	}

	b.WriteString("\n" + dimStyle.Render(m.hint()))
	return b.String()
}

// hint is the key legend under the list. It changes with the mode: search has
// its own keys, and there is no point offering [a]/[n] while runes go to the
// query.
func (m Model) hint() string {
	if m.filtering {
		return "[esc] clear filter   [ctrl+w] wipe query   [space] toggle   [enter] save"
	}
	hint := "[space] toggle   [a] all   [n] none   [/] filter   [enter] save   [q] cancel"
	if m.hasOld {
		hint += "   (locked repos already sync)"
	}
	return hint
}

// body renders every row (and its section header) as one line each, and
// returns where each row landed - lineOf[i] is the line holding row i. The
// windowing below is done over lines rather than rows because a header, and
// the blank line before it, take up screen height too.
func (m Model) body() (lines []string, lineOf []int) {
	vis := m.visible()
	lineOf = make([]int, len(vis))
	current := section(-1)
	for pos, ri := range vis {
		r := m.rows[ri]
		// Headers are noise on a first install, where everything is new.
		if r.section != current {
			current = r.section
			if m.hasOld {
				if pos > 0 {
					lines = append(lines, "")
				}
				lines = append(lines, "  "+dimStyle.Render(sectionLabel[current]))
			}
		}

		box := "[ ]"
		if r.ticked {
			box = "[x]"
		}
		meta := describe(r.repo, r.section)
		if r.locked {
			// Already installed: keep it ticked and say it can't be changed,
			// while still showing the remote/commit detail beside it.
			box = "[x]"
			meta = dimStyle.Render("locked · ") + meta
		}
		line := fmt.Sprintf("%s %-32s %s", box, r.repo.Rel, meta)
		if pos == m.cursor {
			line = selectedStyle.Render("> " + line)
		} else {
			line = "  " + line
		}
		lineOf[pos] = len(lines)
		lines = append(lines, line)
	}
	return lines, lineOf
}

// window is the slice of body lines to draw. It exists because bubbletea's
// renderer drops lines off the *top* of any view taller than the terminal:
// an unwindowed list quietly takes the title and the highlighted row with it,
// which looks exactly like a picker whose cursor and spacebar are dead.
//
// Scrolling is by whole pages, keyed off the cursor alone, so there is no
// scroll offset to keep in sync with the selection - and rows hold still
// between page turns instead of sliding under a centred cursor.
func (m Model) window(total, cursorLine int) (start, end int) {
	// title, its blank line, the blank line and hint below, and one line
	// spare so the terminal does not scroll on the last row.
	chrome := 5
	if m.filtering {
		chrome += 2 // the "/query" line and its blank line
	}
	avail := m.height - chrome
	if m.height <= 0 || total <= avail {
		return 0, total
	}
	avail -= 2 // the "more above" / "more below" counters
	if avail < 1 {
		avail = 1
	}
	start = (cursorLine / avail) * avail
	end = start + avail
	if end > total {
		end = total
	}
	return start, end
}

// countRows is how many rows - not lines - fall in [from, to), so the
// scrolled-off counters talk about repos and not about headers and blanks.
func countRows(lineOf []int, from, to int) int {
	n := 0
	for _, at := range lineOf {
		if at >= from && at < to {
			n++
		}
	}
	return n
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
