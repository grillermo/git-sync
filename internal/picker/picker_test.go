package picker_test

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/grillermo/git-sync/internal/picker"
	"github.com/grillermo/git-sync/internal/scan"
)

func found(rels ...string) []scan.Repo {
	out := make([]scan.Repo, len(rels))
	for i, r := range rels {
		out[i] = scan.Repo{Rel: r, Commits: 10 + i, Remote: "origin", RemoteURL: "u"}
	}
	return out
}

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func sized(m picker.Model) picker.Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return next.(picker.Model)
}

func TestFirstInstallEverythingIsNewAndUnticked(t *testing.T) {
	m := picker.New(found("notes", "work/api"), nil)
	if got := m.Selected(); len(got) != 0 {
		t.Errorf("Selected() = %v, want nothing ticked on a first install", got)
	}
}

func TestNewReposComeFirstAndAlreadySyncingArePreTicked(t *testing.T) {
	// notes and work/api are already syncing; zzz-new is new. Despite sorting
	// last alphabetically, the new one must appear first.
	m := sized(picker.New(found("notes", "work/api", "zzz-new"), []string{"notes", "work/api"}))

	if got := m.RelAt(0); got != "zzz-new" {
		t.Errorf("first row = %q, want the new repo", got)
	}
	sel := m.Selected()
	if len(sel) != 2 || sel[0] != "notes" || sel[1] != "work/api" {
		t.Errorf("Selected() = %v, want the two already-syncing repos pre-ticked", sel)
	}

	view := m.View()
	if !strings.Contains(view, "NEW") || !strings.Contains(view, "ALREADY SYNCING") {
		t.Errorf("view should label both sections:\n%s", view)
	}
	if strings.Index(view, "NEW") > strings.Index(view, "ALREADY SYNCING") {
		t.Errorf("NEW must come before ALREADY SYNCING:\n%s", view)
	}
}

func TestFirstInstallOmitsSectionHeaders(t *testing.T) {
	m := sized(picker.New(found("notes", "work/api"), nil))
	if strings.Contains(m.View(), "ALREADY SYNCING") {
		t.Errorf("nothing is syncing yet, so that header is noise:\n%s", m.View())
	}
}

func TestSpaceTogglesTheHighlightedRepo(t *testing.T) {
	m := sized(picker.New(found("notes", "work/api"), nil))
	m = apply(t, m, "space")
	if got := m.Selected(); len(got) != 1 || got[0] != "notes" {
		t.Errorf("Selected() = %v, want [notes]", got)
	}
	m = apply(t, m, "space")
	if got := m.Selected(); len(got) != 0 {
		t.Errorf("Selected() = %v, want it toggled back off", got)
	}
}

func TestArrowsMoveTheHighlight(t *testing.T) {
	m := sized(picker.New(found("notes", "work/api"), nil))
	m = apply(t, m, "down", "space")
	if got := m.Selected(); len(got) != 1 || got[0] != "work/api" {
		t.Errorf("Selected() = %v, want [work/api]", got)
	}
}

func TestHighlightDoesNotRunOffEitherEnd(t *testing.T) {
	m := sized(picker.New(found("a", "b"), nil))
	m = apply(t, m, "up", "up", "space")
	if got := m.Selected(); len(got) != 1 || got[0] != "a" {
		t.Errorf("Selected() = %v, want [a] - up past the top should clamp", got)
	}
	m = apply(t, m, "down", "down", "down", "space")
	if got := m.Selected(); len(got) != 2 {
		t.Errorf("Selected() = %v, want both - down past the end should clamp", got)
	}
}

func TestSelectAllAndNone(t *testing.T) {
	m := sized(picker.New(found("a", "b", "c"), nil))
	m = apply(t, m, "a")
	if got := m.Selected(); len(got) != 3 {
		t.Errorf("Selected() = %v, want all three", got)
	}
	m = apply(t, m, "n")
	if got := m.Selected(); len(got) != 0 {
		t.Errorf("Selected() = %v, want none", got)
	}
}

func TestEnterConfirms(t *testing.T) {
	m := sized(picker.New(found("a"), nil))
	m = apply(t, m, "space")
	next, cmd := m.Update(key("enter"))
	m = next.(picker.Model)
	if !m.Confirmed() {
		t.Error("enter should confirm")
	}
	if m.Cancelled() {
		t.Error("enter must not also cancel")
	}
	if cmd == nil {
		t.Error("enter should quit the program")
	}
}

func TestQuitCancels(t *testing.T) {
	m := sized(picker.New(found("a"), nil))
	next, cmd := m.Update(key("q"))
	m = next.(picker.Model)
	if !m.Cancelled() {
		t.Error("q should cancel")
	}
	if m.Confirmed() {
		t.Error("q must not confirm")
	}
	if cmd == nil {
		t.Error("q should quit the program")
	}
}

func TestMissingReposAreShownAndPreTicked(t *testing.T) {
	// work/gone is in the config but the scan did not find it - an unmounted
	// volume, say. It must not be silently dropped.
	m := sized(picker.New(found("notes"), []string{"notes", "work/gone"}))
	if !strings.Contains(m.View(), "MISSING") {
		t.Errorf("view should have a MISSING section:\n%s", m.View())
	}
	sel := m.Selected()
	if len(sel) != 2 {
		t.Errorf("Selected() = %v, want the missing repo kept by default", sel)
	}
}

func TestSelectedIsSortedAndStable(t *testing.T) {
	// The result goes straight into config.toml; keep it deterministic.
	m := sized(picker.New(found("zzz", "aaa", "mmm"), nil))
	m = apply(t, m, "a")
	got := m.Selected()
	want := []string{"aaa", "mmm", "zzz"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Selected() = %v, want %v", got, want)
		}
	}
}

func TestRowShowsTheRemoteItWouldSyncThrough(t *testing.T) {
	// Which remote carries the sync is the first thing that decides whether
	// ticking this repo does anything, so it is in the row.
	rows := []scan.Repo{{Rel: "gh", Commits: 3, Remote: "github", RemoteURL: "u"}}
	m := sized(picker.New(rows, nil))
	if !strings.Contains(m.View(), "github") {
		t.Errorf("row should name the remote:\n%s", m.View())
	}
}

func TestARepoWithNoRemoteIsFlagged(t *testing.T) {
	rows := []scan.Repo{{Rel: "solo", Commits: 3}}
	m := sized(picker.New(rows, nil))
	if !strings.Contains(m.View(), "no remote") {
		t.Errorf("a repo that cannot sync must say why:\n%s", m.View())
	}
	// Still tickable: `git remote add` is the fix, and the user may be about
	// to do it. Warning, not a veto.
	m = apply(t, m, "space")
	if len(m.Selected()) != 1 {
		t.Error("a remoteless repo should still be selectable")
	}
}

func TestSnapshotsAreIndependentAfterUpdate(t *testing.T) {
	// Model is a value type; Update must not mutate the backing array shared
	// with a saved copy, or "independent snapshot" semantics silently break.
	m1 := sized(picker.New(found("notes", "work/api"), nil))
	m2 := m1
	m1 = apply(t, m1, "space")

	if got := m1.Selected(); len(got) != 1 || got[0] != "notes" {
		t.Errorf("m1.Selected() = %v, want [notes]", got)
	}
	if got := m2.Selected(); len(got) != 0 {
		t.Errorf("m2.Selected() = %v, want unaffected by m1's toggle", got)
	}
}

func TestMissingRowSaysNotFoundNotNoRemote(t *testing.T) {
	// A MISSING row has no remote data at all (that info is not in the
	// selected []string param), so it must not be blamed on a missing
	// remote - the real reason is it wasn't found on disk this scan.
	m := sized(picker.New(found("notes"), []string{"notes", "work/gone"}))
	view := m.View()
	if strings.Contains(view, "no remote") {
		t.Errorf("MISSING row should not say 'no remote':\n%s", view)
	}
	if !strings.Contains(view, "not found on disk") {
		t.Errorf("MISSING row should say it wasn't found on disk:\n%s", view)
	}
}

func TestEmptyScanDoesNotPanic(t *testing.T) {
	m := sized(picker.New(nil, nil))
	if view := m.View(); !strings.Contains(view, "No git repos") {
		t.Errorf("empty picker should say so:\n%s", view)
	}
	next, _ := m.Update(key("space"))
	_ = next.(picker.Model).View()
}

func apply(t *testing.T, m picker.Model, keys ...string) picker.Model {
	t.Helper()
	for _, k := range keys {
		next, _ := m.Update(key(k))
		m = next.(picker.Model)
	}
	return m
}
