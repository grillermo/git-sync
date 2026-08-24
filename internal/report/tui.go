package report

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/grillermo/git-sync/internal/activity"
)

const listWidth = 32

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
)

// Model is the report TUI: a repo list on the left, that repo's history on
// the right. Exported so it can be driven directly from tests.
type Model struct {
	all      []Summary // every repo, unfiltered
	shown    []Summary // what the list currently displays
	cursor   int
	detail   viewport.Model
	width    int
	height   int
	problems bool // problems-only filter
	ready    bool
}

func NewModel(summaries []Summary) Model {
	m := Model{all: summaries}
	m.applyFilter()
	return m
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) SelectedRepo() string {
	if len(m.shown) == 0 {
		return ""
	}
	return m.shown[m.cursor].Repo
}

func (m Model) ProblemsOnly() bool { return m.problems }

func (m *Model) applyFilter() {
	// A fresh slice, not m.shown[:0]: bubbletea passes the model by value, so
	// reusing the backing array would mutate the previous model's view of it.
	shown := make([]Summary, 0, len(m.all))
	for _, s := range m.all {
		if m.problems && s.Problems == 0 {
			continue
		}
		shown = append(shown, s)
	}
	m.shown = shown
	if m.cursor >= len(m.shown) {
		m.cursor = max(0, len(m.shown)-1)
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Clamp: a narrow terminal must not produce a negative pane width.
		detailW := max(10, msg.Width-listWidth-3)
		detailH := max(3, msg.Height-4)
		if !m.ready {
			m.detail = viewport.New(detailW, detailH)
			m.ready = true
		} else {
			m.detail.Width, m.detail.Height = detailW, detailH
		}
		m.detail.SetContent(m.detailContent())

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "down", "j":
			if m.cursor < len(m.shown)-1 {
				m.cursor++
				m.detail.SetContent(m.detailContent())
				m.detail.GotoTop()
			}
			// Return early so the viewport does not also consume this key
			// and scroll the detail pane while you are moving between repos.
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.detail.SetContent(m.detailContent())
				m.detail.GotoTop()
			}
			return m, nil
		case "e":
			m.problems = !m.problems
			m.applyFilter()
			m.detail.SetContent(m.detailContent())
			m.detail.GotoTop()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg)
	return m, cmd
}

func statusStyle(s activity.Status) lipgloss.Style {
	switch s {
	case activity.StatusError:
		return errStyle
	case activity.StatusWarn:
		return warnStyle
	case activity.StatusOK:
		return okStyle
	default:
		return dimStyle
	}
}

// detailContent is the selected repo's history, newest first.
func (m Model) detailContent() string {
	if len(m.shown) == 0 {
		return ""
	}
	s := m.shown[m.cursor]
	var b strings.Builder
	for _, e := range s.Events {
		b.WriteString(fmt.Sprintf("%s  %-8s %s  %s\n",
			dimStyle.Render(e.Time.Format("2006-01-02 15:04")),
			e.Op,
			statusStyle(e.Status).Render(fmt.Sprintf("%-5s", e.Status)),
			e.Msg))
	}
	return b.String()
}

func (m Model) listView() string {
	// Clamp every row to the column. fmt pads by rune count, so a name made of
	// wide characters (CJK, emoji) renders past listWidth even after
	// truncation - and the Width() in View would then *wrap* that row, shearing
	// the two panes out of alignment for every row below it. MaxWidth cuts
	// instead of wrapping, and is ANSI-aware so it will not eat a style reset.
	clamp := lipgloss.NewStyle().MaxWidth(listWidth)
	var b strings.Builder
	for i, s := range m.shown {
		line := fmt.Sprintf("%-20s %4d", truncate(s.Repo, 20), s.Total)
		if s.Problems > 0 {
			line += warnStyle.Render(fmt.Sprintf(" !%d", s.Problems))
		}
		if i == m.cursor {
			line = selectedStyle.Render("> " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(clamp.Render(line))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) View() string {
	// Guard both empty cases before touching the cursor or the viewport.
	if len(m.all) == 0 {
		return titleStyle.Render("git-sync") + "\n\n" +
			"No activity recorded yet.\n\n" +
			dimStyle.Render("Commit in a repo under your base_dir and it will show up here.\n") +
			dimStyle.Render("Press q to quit.")
	}
	if len(m.shown) == 0 {
		return titleStyle.Render("git-sync") + "\n\n" +
			"No problems recorded. Press e to show everything, q to quit."
	}

	t := Totalize(m.shown)
	header := titleStyle.Render(fmt.Sprintf("git-sync  %d repos  %d events  %d problems",
		t.Repos, t.Events, t.Problems))
	if m.problems {
		header += warnStyle.Render("  [problems only]")
	}

	left := lipgloss.NewStyle().Width(listWidth).Render(m.listView())
	right := lipgloss.NewStyle().Render(m.detail.View())
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, dimStyle.Render(" | "), right)

	help := dimStyle.Render("up/down repo  ·  e problems  ·  q quit")
	return header + "\n" + body + "\n" + help
}

// truncate shortens s to at most n characters, marking the cut with an
// ellipsis. It counts runes rather than bytes: repo names come from the
// filesystem, and slicing a multi-byte one mid-rune emits invalid UTF-8 that
// the terminal then renders as a replacement character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:max(0, n)])
	}
	return string(r[:n-3]) + "..."
}
