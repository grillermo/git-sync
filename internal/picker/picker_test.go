package picker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/grillermo/chicle"

	"github.com/grillermo/git-sync/internal/scan"
)

func found(rels ...string) []scan.Repo {
	out := make([]scan.Repo, len(rels))
	for i, r := range rels {
		out[i] = scan.Repo{Rel: r, Commits: 10 + i, Remote: "origin", RemoteURL: "u"}
	}
	return out
}

func keys(rows []chicle.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Key
	}
	return out
}

func rowFor(t *testing.T, rows []chicle.Row, rel string) chicle.Row {
	t.Helper()
	for _, r := range rows {
		if r.Key == rel {
			return r
		}
	}
	t.Fatalf("no row for %q in %v", rel, keys(rows))
	return chicle.Row{}
}

func text(r chicle.Row) string { return strings.Join(r.Cols, " ") }

func TestFirstInstallEverythingIsUntickedAndUnsectioned(t *testing.T) {
	rows := Rows(found("notes", "work/api"), nil)
	for _, r := range rows {
		if r.Ticked || r.Locked {
			t.Errorf("%s: ticked=%v locked=%v, want neither on a first install", r.Key, r.Ticked, r.Locked)
		}
		if r.Section != "" {
			t.Errorf("%s: section %q, want none so chicle draws no headings", r.Key, r.Section)
		}
	}
}

func TestNewReposComeFirstAndAlreadySyncingAreLockedAndTicked(t *testing.T) {
	// zzz-new sorts last alphabetically but is the only thing that changed.
	rows := Rows(found("notes", "work/api", "zzz-new"), []string{"notes", "work/api"})

	if got, want := keys(rows), []string{"zzz-new", "notes", "work/api"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if r := rowFor(t, rows, "zzz-new"); r.Ticked || r.Locked || r.Section != "NEW" {
		t.Errorf("zzz-new = %+v, want unticked, unlocked, NEW", r)
	}
	for _, rel := range []string{"notes", "work/api"} {
		if r := rowFor(t, rows, rel); !r.Ticked || !r.Locked || r.Section != "ALREADY SYNCING" {
			t.Errorf("%s = %+v, want ticked, locked, ALREADY SYNCING", rel, r)
		}
	}
}

func TestMissingReposAreKeptTickedButNotLocked(t *testing.T) {
	// work/gone is in the config but the scan did not find it - an unmounted
	// volume, say. It must not be silently dropped.
	rows := Rows(found("notes"), []string{"notes", "work/gone"})

	r := rowFor(t, rows, "work/gone")
	if !r.Ticked {
		t.Error("a missing repo should stay ticked so saving does not drop it")
	}
	if r.Locked {
		t.Error("a missing repo should be removable: the user may have deleted it on purpose")
	}
	if !strings.HasPrefix(r.Section, "MISSING") {
		t.Errorf("section = %q, want MISSING", r.Section)
	}
	if !strings.Contains(text(r), "not found on disk") {
		t.Errorf("a missing row should say why: %q", text(r))
	}
	if strings.Contains(text(r), "no remote") {
		t.Errorf("a missing row has no remote data, it must not be blamed on one: %q", text(r))
	}
	if got := keys(rows); got[len(got)-1] != "work/gone" {
		t.Errorf("order = %v, want MISSING last", got)
	}
}

func TestRowNamesTheRemoteItWouldSyncThrough(t *testing.T) {
	rows := Rows([]scan.Repo{{Rel: "gh", Commits: 3, Remote: "github", RemoteURL: "u"}}, nil)
	if got := text(rows[0]); !strings.Contains(got, "github") || !strings.Contains(got, "3 commits") {
		t.Errorf("row should name the remote and commit count: %q", got)
	}
}

func TestARepoWithNoRemoteIsFlaggedButStillPickable(t *testing.T) {
	rows := Rows([]scan.Repo{{Rel: "solo", Commits: 3}}, nil)
	if got := text(rows[0]); !strings.Contains(got, "no remote") {
		t.Errorf("a repo that cannot sync must say why: %q", got)
	}
	if rows[0].Locked {
		t.Error("a remoteless repo is a warning, not a veto")
	}
}

func TestRowsAreInOneColumnPerConfigColumn(t *testing.T) {
	cfg := Config(found("a"), nil)
	for _, r := range cfg.Rows {
		if len(r.Cols) != len(cfg.Columns) {
			t.Errorf("%s has %d cells for %d columns", r.Key, len(r.Cols), len(cfg.Columns))
		}
	}
}

func run(t *testing.T, cfg chicle.Config, label string, ticked ...string) chicle.Outcome {
	t.Helper()
	sel := chicle.Selection{}
	for _, rel := range ticked {
		sel.Ticked = append(sel.Ticked, chicle.Row{Key: rel})
	}
	for _, a := range cfg.Actions {
		if a.Label == label {
			if a.Run == nil {
				return chicle.Outcome{Done: true}
			}
			return a.Run(sel)
		}
	}
	t.Fatalf("no %q action in %v", label, cfg.Actions)
	return chicle.Outcome{}
}

func TestSavingReturnsTheTickedReposSorted(t *testing.T) {
	// The result goes straight into config.toml; keep it deterministic.
	cfg := Config(found("zzz", "aaa", "mmm"), nil)
	out := run(t, cfg, "Save", "zzz", "aaa", "mmm")
	if !out.Done {
		t.Fatal("Save must end the picker")
	}
	got, ok := decode(out.Result)
	if want := []string{"aaa", "mmm", "zzz"}; !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("decode = %v, %v; want %v, true", got, ok, want)
	}
}

func TestSavingWithNothingTickedIsNotACancel(t *testing.T) {
	out := run(t, Config(found("a"), nil), "Save")
	got, ok := decode(out.Result)
	if !ok || len(got) != 0 {
		t.Errorf("decode = %v, %v; want an empty, non-cancelled selection", got, ok)
	}
}

func TestCancelDecodesAsCancelled(t *testing.T) {
	out := run(t, Config(found("a"), nil), "Cancel")
	if _, ok := decode(out.Result); ok {
		t.Error("Cancel must not read as a save")
	}
	if _, ok := decode(""); ok {
		t.Error("quitting with q/esc returns an empty result and must not read as a save")
	}
}

func TestPickerIsMultiSelect(t *testing.T) {
	if !Config(found("a"), nil).MultiSelect {
		t.Error("choosing repos needs checkboxes")
	}
}

func TestEmptyScanWithNothingConfiguredIsAnError(t *testing.T) {
	_, _, err := choose(func(chicle.Config) (string, error) {
		t.Fatal("there is nothing to pick, the UI must not open")
		return "", nil
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "No git repos") {
		t.Errorf("err = %v, want a message saying no repos were found", err)
	}
}

func TestChooseReturnsWhatTheUIReturned(t *testing.T) {
	sel, ok, err := choose(func(cfg chicle.Config) (string, error) {
		return encode([]string{"b", "a"}), nil
	}, found("a", "b"), nil)
	if err != nil || !ok || !reflect.DeepEqual(sel, []string{"a", "b"}) {
		t.Errorf("choose = %v, %v, %v; want [a b], true, nil", sel, ok, err)
	}
}

func TestChooseReportsCancel(t *testing.T) {
	_, ok, err := choose(func(chicle.Config) (string, error) { return "", nil }, found("a"), nil)
	if err != nil || ok {
		t.Errorf("ok=%v err=%v, want a clean cancel", ok, err)
	}
}
