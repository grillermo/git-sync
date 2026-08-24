package scan_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/scan"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestFindsReposAtSeveralDepths(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("notes")
	sb.MakeRepo("work/api")
	sb.MakeRepo("work/deep/nested/thing")

	got, err := scan.Repos(sb.BaseDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"notes", "work/api", "work/deep/nested/thing"}
	if len(got) != len(want) {
		t.Fatalf("found %d repos (%v), want %d", len(got), paths(got), len(want))
	}
	for i, w := range want {
		if got[i].Rel != w {
			t.Errorf("repo %d = %q, want %q (results must be sorted)", i, got[i].Rel, w)
		}
	}
}

func TestDoesNotDescendIntoARepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	outer := sb.MakeRepo("outer")
	// A repo checked out inside another repo is part of the outer one as far
	// as syncing is concerned; it must not appear as its own entry.
	inner := filepath.Join(outer, "vendor", "inner")
	testutil.MkdirAll(t, inner)
	sb.Git(sb.Home, "init", "-q", inner)

	got, err := scan.Repos(sb.BaseDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Rel != "outer" {
		t.Errorf("got %v, want just [outer]", paths(got))
	}
}

func TestSkipsDotDirectories(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("visible")
	hidden := filepath.Join(sb.BaseDir, ".cache", "hidden")
	testutil.MkdirAll(t, hidden)
	sb.Git(sb.Home, "init", "-q", hidden)

	got, _ := scan.Repos(sb.BaseDir, nil)
	if len(got) != 1 || got[0].Rel != "visible" {
		t.Errorf("got %v, want just [visible]", paths(got))
	}
}

func TestFindsAWorktreeWhereDotGitIsAFile(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	wt := filepath.Join(sb.BaseDir, "proj-wt")
	sb.Git(repo, "worktree", "add", "-q", wt, "-b", "wt")

	got, _ := scan.Repos(sb.BaseDir, nil)
	if len(got) != 2 {
		t.Errorf("got %v, want both the repo and its linked worktree", paths(got))
	}
}

func TestCollectsCommitCountAndLastCommit(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	testutil.Commit(t, sb, repo, "second")

	got, _ := scan.Repos(sb.BaseDir, nil)
	if len(got) != 1 {
		t.Fatalf("got %v", paths(got))
	}
	if got[0].Commits != 2 {
		t.Errorf("Commits = %d, want 2", got[0].Commits)
	}
	if time.Since(got[0].LastCommit) > time.Minute {
		t.Errorf("LastCommit = %v, want ~now", got[0].LastCommit)
	}
}

func TestHandlesARepoWithNoCommits(t *testing.T) {
	sb := testutil.NewSandbox(t)
	empty := filepath.Join(sb.BaseDir, "empty")
	testutil.MkdirAll(t, empty)
	sb.Git(sb.Home, "init", "-q", empty)

	got, err := scan.Repos(sb.BaseDir, nil)
	if err != nil {
		t.Fatalf("an empty repo must not fail the scan: %v", err)
	}
	if len(got) != 1 || got[0].Commits != 0 {
		t.Errorf("got %+v, want one repo with 0 commits", got)
	}
	if !got[0].LastCommit.IsZero() {
		t.Error("a repo with no commits should have a zero LastCommit")
	}
}

func TestRecordsTheRemoteEachRepoWouldSyncThrough(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("plain")                       // origin
	sb.MakeRepoNamedRemote("gh", "github")     // github
	local := filepath.Join(sb.BaseDir, "solo") // no remote at all
	testutil.MkdirAll(t, local)
	sb.Git(sb.Home, "init", "-q", local)

	got, _ := scan.Repos(sb.BaseDir, nil)
	byRel := map[string]scan.Repo{}
	for _, r := range got {
		byRel[r.Rel] = r
	}
	if byRel["plain"].Remote != "origin" {
		t.Errorf("plain.Remote = %q, want origin", byRel["plain"].Remote)
	}
	if byRel["gh"].Remote != "github" {
		t.Errorf("gh.Remote = %q, want github", byRel["gh"].Remote)
	}
	if byRel["solo"].CanSync() {
		t.Error("a repo with no remote cannot sync and must say so")
	}
	if byRel["plain"].RemoteURL == "" {
		t.Error("RemoteURL is what the two machines must agree on; record it")
	}
}

func TestHonoursTheRemotePreferenceOrder(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepoNamedRemote("odd", "gitlab")

	got, _ := scan.Repos(sb.BaseDir, []string{"gitlab"})
	if len(got) != 1 || got[0].Remote != "gitlab" {
		t.Errorf("got %+v, want the configured remote name", got)
	}
}

func TestEmptyBaseDir(t *testing.T) {
	sb := testutil.NewSandbox(t)
	got, err := scan.Repos(sb.BaseDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", paths(got))
	}
}

func paths(rs []scan.Repo) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Rel
	}
	return out
}
