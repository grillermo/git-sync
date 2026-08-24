package gitcmd_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestRunReturnsTrimmedOutput(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	out, err := gitcmd.Run(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if out != "main" {
		t.Errorf("branch = %q, want %q (output should be trimmed)", out, "main")
	}
}

func TestRunErrorIncludesGitsStderr(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	_, err := gitcmd.Run(repo, "checkout", "no-such-branch")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no-such-branch") {
		t.Errorf("error %q should quote what git said", err)
	}
}

func TestIsDirty(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")

	dirty, err := gitcmd.IsDirty(repo)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Error("a fresh clone should be clean")
	}

	sb.Dirty(repo)
	dirty, err = gitcmd.IsDirty(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Error("IsDirty should see an untracked file, not just tracked edits")
	}
}

func TestCurrentBranchDetachedHead(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "checkout", "-q", "--detach")

	_, err := gitcmd.CurrentBranch(repo)
	if !gitcmd.IsDetachedHead(err) {
		t.Errorf("err = %v, want a detached-head error", err)
	}
}

func TestHasRemoteBranch(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	if !gitcmd.HasRemoteBranch(repo, "origin", "main") {
		t.Error("main was pushed, so origin/main exists")
	}
	if gitcmd.HasRemoteBranch(repo, "origin", "orphan") {
		t.Error("a branch never pushed has no remote counterpart")
	}
}

func TestResolveRemotePrefersGithubOverOrigin(t *testing.T) {
	// The whole sync goes through this remote, so when a repo has both, the
	// two machines must pick the same one.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "remote", "add", "github", "https://example.invalid/proj.git")

	got, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "github" {
		t.Errorf("ResolveRemote = %q, want %q", got, "github")
	}
}

func TestResolveRemoteFallsBackToOrigin(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj") // origin only
	got, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "origin" {
		t.Errorf("ResolveRemote = %q, want %q", got, "origin")
	}
}

func TestResolveRemoteAcceptsASingleOddlyNamedRemote(t *testing.T) {
	// One remote and no ambiguity: refusing to sync would be pedantic.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "remote", "rename", "origin", "gitlab")

	got, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "gitlab" {
		t.Errorf("ResolveRemote = %q, want the repo's only remote", got)
	}
}

func TestResolveRemoteIsAmbiguousWithSeveralUnlistedRemotes(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "remote", "rename", "origin", "gitlab")
	sb.Git(repo, "remote", "add", "backup", "https://example.invalid/b.git")

	if _, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"}); err == nil {
		t.Error("two unlisted remotes is a guess we must not make")
	}
}

func TestResolveRemoteWithNoRemotes(t *testing.T) {
	sb := testutil.NewSandbox(t)
	local := filepath.Join(sb.BaseDir, "local-only")
	testutil.MkdirAll(t, local)
	sb.Git(sb.Home, "init", "-q", local)

	_, err := gitcmd.ResolveRemote(local, []string{"github", "origin"})
	if !gitcmd.IsNoRemote(err) {
		t.Errorf("err = %v, want a no-remote error: this repo can never sync", err)
	}
}

func TestPushNamesTheRemoteAndBranch(t *testing.T) {
	// Not plain `git push`: the branch may track origin while the shared
	// remote is github.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	testutil.Commit(t, sb, repo, "second")

	if _, err := gitcmd.Push(repo, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if out := sb.Git(repo, "log", "--oneline", "origin/main"); !strings.Contains(out, "second") {
		t.Errorf("commit did not reach origin/main:\n%s", out)
	}
}

func TestFastForwardOntoTheNamedRemote(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.PeerClone("proj")
	sb.PeerCommit("proj", "from-peer")

	if err := gitcmd.Fetch(repo, "origin"); err != nil {
		t.Fatal(err)
	}
	if err := gitcmd.FastForward(repo, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if out := sb.Git(repo, "log", "--oneline"); !strings.Contains(out, "from-peer") {
		t.Errorf("did not fast-forward onto origin/main:\n%s", out)
	}
}

func TestToplevel(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sub := filepath.Join(repo, "deep", "nested")
	testutil.MkdirAll(t, sub)

	got, err := gitcmd.Toplevel(sub)
	if err != nil {
		t.Fatal(err)
	}
	// macOS /var -> /private/var, so compare resolved paths.
	if !testutil.SamePath(got, repo) {
		t.Errorf("Toplevel(%q) = %q, want %q", sub, got, repo)
	}
}
