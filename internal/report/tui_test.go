package report_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/report"
)

// keyMsg builds the key events bubbletea would deliver, so the model can be
// driven without a terminal.
func keyMsg(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	panic("unhandled key " + s)
}

// work/api is the more recently active repo and the only one with a problem,
// which is what lets one fixture cover both ordering and filtering.
func demoSummaries() []report.Summary {
	return report.Summarize([]activity.Event{
		ev("work/api", activity.OpPush, activity.StatusOK, 9),
		ev("work/api", activity.OpReceive, activity.StatusWarn, 8),
		ev("personal/notes", activity.OpPush, activity.StatusOK, 5),
	})
}

func TestTUIStartsOnTheFirstRepo(t *testing.T) {
	m := report.NewModel(demoSummaries())
	if got := m.SelectedRepo(); got != "work/api" {
		t.Errorf("selected = %q, want the most recently active repo", got)
	}
}

func TestTUIArrowKeysMoveBetweenRepos(t *testing.T) {
	m := report.NewModel(demoSummaries())
	next, _ := m.Update(keyMsg("down"))
	m = next.(report.Model)
	if got := m.SelectedRepo(); got != "personal/notes" {
		t.Errorf("after down, selected = %q, want personal/notes", got)
	}
	prev, _ := m.Update(keyMsg("up"))
	m = prev.(report.Model)
	if got := m.SelectedRepo(); got != "work/api" {
		t.Errorf("after up, selected = %q, want work/api", got)
	}
}

func TestTUISelectionDoesNotWrapPastTheEnds(t *testing.T) {
	m := report.NewModel(demoSummaries())
	for i := 0; i < 10; i++ {
		next, _ := m.Update(keyMsg("down"))
		m = next.(report.Model)
	}
	if got := m.SelectedRepo(); got != "personal/notes" {
		t.Errorf("selected = %q, want it clamped to the last repo", got)
	}
}

func TestTUIDetailPaneShowsTheSelectedRepoOnly(t *testing.T) {
	m := report.NewModel(demoSummaries())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(report.Model)

	view := m.View()
	if !strings.Contains(view, "work/api") {
		t.Errorf("detail pane should show the selected repo:\n%s", view)
	}
	if !strings.Contains(view, "diverged") && !strings.Contains(view, "warn") {
		t.Errorf("detail pane should show that repo's events:\n%s", view)
	}
}

func TestTUIToggleProblemsFilter(t *testing.T) {
	m := report.NewModel(demoSummaries())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(report.Model)

	toggled, _ := m.Update(keyMsg("e"))
	m = toggled.(report.Model)
	if !m.ProblemsOnly() {
		t.Error("'e' should turn on the problems-only filter")
	}
	// personal/notes has no problems, so it drops out of the list.
	if strings.Contains(m.View(), "personal/notes") {
		t.Errorf("filtered view should hide problem-free repos:\n%s", m.View())
	}

	again, _ := m.Update(keyMsg("e"))
	m = again.(report.Model)
	if m.ProblemsOnly() {
		t.Error("'e' should toggle back off")
	}
}

func TestTUIQuits(t *testing.T) {
	m := report.NewModel(demoSummaries())
	_, cmd := m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("'q' should return a command")
	}
	if msg := cmd(); msg == nil {
		t.Error("'q' should produce tea.Quit")
	}
}

func TestTUIHandlesEmptyHistory(t *testing.T) {
	m := report.NewModel(nil)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(report.Model)
	// Must not panic, and must say something useful.
	if view := m.View(); !strings.Contains(view, "No activity") {
		t.Errorf("empty TUI should explain itself:\n%s", view)
	}
}

// Repo names are filesystem paths, so they are not necessarily ASCII. A wide
// (CJK, emoji) name stresses two separate things: cutting it by byte leaves
// invalid UTF-8, and letting it render past the list column makes lipgloss
// wrap that row, which shears the two panes apart for every row below it.
func TestTUIWideRepoNamesDoNotBreakTheLayout(t *testing.T) {
	// Built literally rather than via ev(), whose derived Msg would repeat the
	// repo name in the detail pane and defeat the "was it shortened" check.
	long := "日本語のリポジトリ/深いサブディレクトリ/api"
	m := report.NewModel(report.Summarize([]activity.Event{
		{Time: t1, Repo: long, Op: activity.OpPush, Status: activity.StatusOK, Msg: "pushed"},
	}))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(report.Model)

	view := m.View()
	if !utf8.ValidString(view) {
		t.Errorf("view is not valid UTF-8, a name was cut mid-rune:\n%q", view)
	}
	// It really was shortened, otherwise the assertions here prove nothing.
	if strings.Contains(view, long) {
		t.Errorf("a name longer than the column should be truncated:\n%s", view)
	}
	// The panes are joined row by row, so the sole repo row and the sole event
	// row have to land on the same line. They do not if the name wrapped.
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "pushed") {
			if !strings.Contains(line, "日本語") {
				t.Errorf("repo row and its event row came apart, the list wrapped:\n%s", view)
			}
			return
		}
	}
	t.Errorf("never found the event row:\n%s", view)
}

func TestTUISurvivesATinyTerminal(t *testing.T) {
	m := report.NewModel(demoSummaries())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	m = next.(report.Model)
	_ = m.View() // must not panic on a negative computed pane width
}
