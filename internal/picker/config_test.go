package picker_test

import (
	"strings"
	"testing"

	"github.com/grillermo/chicle"
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

func keys(rows []chicle.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Key
	}
	return out
}

func TestConfigFirstInstallEverythingIsNewAndUnticked(t *testing.T) {
	cfg := picker.Config(found("notes", "work/api"), nil)
	if len(cfg.Rows) != 2 {
		t.Fatalf("Rows = %v, want 2", cfg.Rows)
	}
	for _, r := range cfg.Rows {
		if r.Ticked || r.Locked {
			t.Errorf("row %q should be unticked and unlocked on a first install", r.Key)
		}
		if r.Section != "NEW" {
			t.Errorf("row %q section = %q, want NEW", r.Key, r.Section)
		}
	}
}

func TestConfigNewReposComeFirstAndAlreadySyncingAreLocked(t *testing.T) {
	// notes and work/api are already syncing; zzz-new is new. Despite sorting
	// last alphabetically, the new one must appear first.
	cfg := picker.Config(found("notes", "work/api", "zzz-new"), []string{"notes", "work/api"})

	got := keys(cfg.Rows)
	want := []string{"zzz-new", "notes", "work/api"}
	if len(got) != len(want) {
		t.Fatalf("Rows keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Rows keys = %v, want %v", got, want)
		}
	}

	for _, r := range cfg.Rows {
		switch r.Key {
		case "zzz-new":
			if r.Locked || r.Section != "NEW" {
				t.Errorf("zzz-new should be unlocked/NEW, got locked=%v section=%q", r.Locked, r.Section)
			}
		case "notes", "work/api":
			if !r.Locked || r.Section != "ALREADY SYNCING" {
				t.Errorf("%s should be locked/ALREADY SYNCING, got locked=%v section=%q", r.Key, r.Locked, r.Section)
			}
		}
	}
}

func TestConfigMissingReposAreKeptLockedUnderTheirOwnSection(t *testing.T) {
	// work/gone is in the config but the scan did not find it - an unmounted
	// volume, say. It must not be silently dropped.
	cfg := picker.Config(found("notes"), []string{"notes", "work/gone"})

	var gone *chicle.Row
	for i, r := range cfg.Rows {
		if r.Key == "work/gone" {
			gone = &cfg.Rows[i]
		}
	}
	if gone == nil {
		t.Fatalf("Rows = %v, want work/gone kept", cfg.Rows)
	}
	if !gone.Locked {
		t.Error("a missing repo must stay locked (ticked, un-droppable)")
	}
	if gone.Section != "MISSING (in your config, not found on disk)" {
		t.Errorf("missing repo section = %q, want the MISSING label", gone.Section)
	}
	if gone.Cols[1] != "not found on disk" {
		t.Errorf("missing repo description = %q, want 'not found on disk'", gone.Cols[1])
	}
}

func TestConfigDescribesTheRemoteItWouldSyncThrough(t *testing.T) {
	rows := []scan.Repo{{Rel: "gh", Commits: 3, Remote: "github", RemoteURL: "u"}}
	cfg := picker.Config(rows, nil)
	if got := cfg.Rows[0].Cols[1]; got == "" || !strings.Contains(got, "github") {
		t.Errorf("description = %q, want it to name the remote", got)
	}
}

func TestConfigFlagsARepoWithNoRemote(t *testing.T) {
	rows := []scan.Repo{{Rel: "solo", Commits: 3}}
	cfg := picker.Config(rows, nil)
	if got := cfg.Rows[0].Cols[1]; !strings.Contains(got, "no remote") {
		t.Errorf("description = %q, want a no-remote warning", got)
	}
	// Still tickable: `git remote add` is the fix. Not locked, no ticked flag
	// forced, and definitely not dropped from the list.
	if cfg.Rows[0].Locked {
		t.Error("a remoteless repo should still be selectable, not locked")
	}
}

func TestConfigTitleAndMultiSelect(t *testing.T) {
	cfg := picker.Config(nil, nil)
	if cfg.Title != "SELECT REPOS TO SYNC" {
		t.Errorf("Title = %q", cfg.Title)
	}
	if !cfg.MultiSelect {
		t.Error("MultiSelect must be on: this is a checkbox list")
	}
}

func TestSelectedIsSortedRegardlessOfRowOrder(t *testing.T) {
	// The result goes straight into config.toml; keep it deterministic even
	// though chicle hands back Ticked in row order, not sorted.
	sel := chicle.Selection{Ticked: []chicle.Row{
		{Key: "zzz"}, {Key: "aaa"}, {Key: "mmm"},
	}}
	got := picker.Selected(sel)
	want := []string{"aaa", "mmm", "zzz"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Selected() = %v, want %v", got, want)
		}
	}
}

func TestSelectedOfEmptyIsEmpty(t *testing.T) {
	got := picker.Selected(chicle.Selection{})
	if len(got) != 0 {
		t.Errorf("Selected(empty) = %v, want none", got)
	}
}
