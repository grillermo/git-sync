package setup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

// stubSSHRunsLocally makes ssh run the clone script it is handed through a
// real local shell, so these tests exercise the script itself, not a canned
// reply.
func stubSSHRunsLocally(sb *testutil.Sandbox) {
	sb.StubSSHScripted(map[string]string{
		"*git-sync-clone*": `for a; do last=$a; done; sh -c "$last"; exit $?`,
	}, 0)
}

func TestClonePeerReposClonesUnderOurRemoteNameAndBranch(t *testing.T) {
	sb := testutil.NewSandbox(t)
	work := sb.MakeRepo("group/proj")
	sb.Git(work, "checkout", "-qb", "feature")
	sb.Git(work, "push", "-q", "origin", "feature")
	stubSSHRunsLocally(sb)

	peerBase := filepath.Join(sb.Home, "peer-code")
	url := filepath.Join(sb.Home, "remotes", "group/proj.git")
	got, err := setup.ClonePeerRepos("t@b.local", peerBase, []setup.RepoWant{
		{Rel: "group/proj", RemoteURL: url, Remote: "github", Branch: "feature"},
	})
	if err != nil {
		t.Fatalf("ClonePeerRepos: %v", err)
	}
	if len(got) != 1 || got[0].Err != "" || got[0].Branch != "feature" {
		t.Fatalf("results = %+v, want group/proj cloned on feature", got)
	}
	dst := filepath.Join(peerBase, "group/proj")
	if remotes := strings.TrimSpace(sb.Git(dst, "remote")); remotes != "github" {
		t.Errorf("clone's remotes = %q, want github - both machines must resolve the same remote", remotes)
	}
	if b := strings.TrimSpace(sb.Git(dst, "symbolic-ref", "--short", "HEAD")); b != "feature" {
		t.Errorf("clone is on %q, want feature", b)
	}
}

func TestClonePeerReposStaysOnTheDefaultBranchWhenOursIsNotPushed(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("proj")
	stubSSHRunsLocally(sb)

	peerBase := filepath.Join(sb.Home, "peer-code")
	got, _ := setup.ClonePeerRepos("t@b.local", peerBase, []setup.RepoWant{{
		Rel: "proj", RemoteURL: filepath.Join(sb.Home, "remotes", "proj.git"),
		Remote: "origin", Branch: "unpushed",
	}})
	if len(got) != 1 || got[0].Err != "" || got[0].Branch != "main" {
		t.Errorf("results = %+v, want a clone left on main", got)
	}
}

func TestClonePeerReposNeverTouchesAnExistingPath(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("proj")
	stubSSHRunsLocally(sb)

	peerBase := filepath.Join(sb.Home, "peer-code")
	testutil.WriteFileIn(t, filepath.Join(peerBase, "proj"), "mine.txt", "keep me\n")
	got, _ := setup.ClonePeerRepos("t@b.local", peerBase, []setup.RepoWant{{
		Rel: "proj", RemoteURL: filepath.Join(sb.Home, "remotes", "proj.git"), Remote: "origin",
	}})
	if len(got) != 1 || !strings.Contains(got[0].Err, "already exists") {
		t.Errorf("results = %+v, want an already-exists error", got)
	}
	if _, err := os.Stat(filepath.Join(peerBase, "proj", ".git")); err == nil {
		t.Error("cloned into a directory that was already there")
	}
}

func TestClonePeerReposReportsAFailedClone(t *testing.T) {
	sb := testutil.NewSandbox(t)
	stubSSHRunsLocally(sb)

	peerBase := filepath.Join(sb.Home, "peer-code")
	got, _ := setup.ClonePeerRepos("t@b.local", peerBase, []setup.RepoWant{
		{Rel: "gone", RemoteURL: filepath.Join(sb.Home, "no-such.git"), Remote: "origin"},
	})
	if len(got) != 1 || !strings.Contains(got[0].Err, "clone from") {
		t.Errorf("results = %+v, want a clone failure naming the URL", got)
	}
	var out strings.Builder
	if n := setup.RenderCloneResults(&out, "b.local", got); n != 1 {
		t.Errorf("RenderCloneResults counted %d failures, want 1:\n%s", n, out.String())
	}
}

func TestClonePeerReposMarksEveryRepoWhenThePeerIsUnreachable(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(nil, 255)

	got, err := setup.ClonePeerRepos("t@down.local", "/home/t/code", []setup.RepoWant{
		{Rel: "a", RemoteURL: "git@example.com:me/a.git", Remote: "origin"},
		{Rel: "b", RemoteURL: "git@example.com:me/b.git", Remote: "origin"},
	})
	if !setup.IsPeerUnreachable(err) {
		t.Errorf("err = %v, want peer unreachable", err)
	}
	for _, r := range got {
		if r.Err == "" {
			t.Errorf("%s reported as cloned on an unreachable peer", r.Rel)
		}
	}
}
