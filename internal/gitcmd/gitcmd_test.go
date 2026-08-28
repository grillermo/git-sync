package gitcmd_test

import (
	"os"
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

func TestDefaultBranchFromCloneHead(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("a")

	got, err := gitcmd.DefaultBranch(repo, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch = %q, want %q", got, "main")
	}
}

func TestDefaultBranchAfterRemoteRename(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepoNamedRemote("a", "github")

	got, err := gitcmd.DefaultBranch(repo, "github")
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch = %q, want %q", got, "main")
	}
}

func TestDefaultBranchSetHeadFallback(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("a")
	sb.AddRemote(t, repo, "github", "a")

	got, err := gitcmd.DefaultBranch(repo, "github")
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch = %q, want %q", got, "main")
	}
}

func TestFastForwardRefAdvancesNonCheckedOutBranch(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("a")
	sb.Git(repo, "checkout", "-qb", "feature")

	sb.PeerClone("a")
	sb.PeerCommit("a", "from-peer")
	if err := gitcmd.Fetch(repo, "origin"); err != nil {
		t.Fatal(err)
	}

	if err := gitcmd.FastForwardRef(repo, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if out := sb.Git(repo, "log", "--oneline", "main"); !strings.Contains(out, "from-peer") {
		t.Errorf("local main was not advanced:\n%s", out)
	}
	if branch, err := gitcmd.CurrentBranch(repo); err != nil || branch != "feature" {
		t.Errorf("current branch = %q, %v; worktree should not move off feature", branch, err)
	}

	// Now diverge: a local commit on main that origin does not have, plus a
	// new commit on origin/main it makes from here.
	sb.Git(repo, "checkout", "-q", "main")
	testutil.Commit(t, sb, repo, "local only")
	sb.Git(repo, "checkout", "-q", "feature")
	sb.PeerCommit("a", "another-from-peer")
	if err := gitcmd.Fetch(repo, "origin"); err != nil {
		t.Fatal(err)
	}
	before := sb.Git(repo, "rev-parse", "main")

	err := gitcmd.FastForwardRef(repo, "origin", "main")
	if err == nil {
		t.Fatal("expected an error on a diverged branch")
	}
	if !gitcmd.IsNotFastForward(err) {
		t.Errorf("err = %v, want a not-fast-forward error", err)
	}
	after := sb.Git(repo, "rev-parse", "main")
	if before != after {
		t.Errorf("main moved despite divergence: %s -> %s", before, after)
	}
}

func TestDefaultBranchUnresolvableRemote(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("a")
	// A remote whose URL doesn't exist: fetch fails, set-head -a has nothing
	// to work from, and refs/remotes/ghost/HEAD is never set.
	sb.Git(repo, "remote", "add", "ghost", filepath.Join(sb.Home, "no-such-remote.git"))

	_, err := gitcmd.DefaultBranch(repo, "ghost")
	if err == nil {
		t.Fatal("expected an error for an unresolvable remote")
	}
	if !gitcmd.IsNoDefaultBranch(err) {
		t.Errorf("err = %v, want a no-default-branch error", err)
	}
}

func TestMergeConflictThenAbortLeavesTreeClean(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("a")

	// Diverge README.md on both sides, at the same base revision, so the
	// merge conflicts. PeerCommit deliberately writes to a different file
	// (PEER.md) to avoid this exact collision, so edit README.md directly.
	testutil.Commit(t, sb, repo, "local change")
	peer := sb.PeerClone("a")
	testutil.AppendFileIn(t, peer, "README.md", "peer change\n")
	sb.Git(peer, "add", "-A")
	sb.Git(peer, "commit", "-qm", "peer change")
	sb.Git(peer, "push", "-q")
	if err := gitcmd.Fetch(repo, "origin"); err != nil {
		t.Fatal(err)
	}

	err := gitcmd.Merge(repo, "origin", "main")
	if err == nil {
		t.Fatal("expected a merge conflict")
	}

	if err := gitcmd.MergeAbort(repo); err != nil {
		t.Fatalf("MergeAbort: %v", err)
	}

	if out := sb.Git(repo, "status", "--porcelain"); out != "" {
		t.Errorf("working tree not clean after MergeAbort:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Errorf("MERGE_HEAD still present after MergeAbort: %v", err)
	}
}

func TestHasLocalBranch(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("a")

	if !gitcmd.HasLocalBranch(repo, "main") {
		t.Error("main is a local branch")
	}
	if gitcmd.HasLocalBranch(repo, "no-such-branch") {
		t.Error("no-such-branch does not exist")
	}
}

func TestAheadBehindCountsBothDirections(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")

	ahead, behind, err := gitcmd.AheadBehind(repo, "origin", "main")
	if err != nil || ahead != 0 || behind != 0 {
		t.Fatalf("fresh clone: got %d/%d, %v; want 0/0, nil", ahead, behind, err)
	}

	// One commit here that origin does not have.
	testutil.Commit(t, sb, repo, "local only")
	ahead, behind, err = gitcmd.AheadBehind(repo, "origin", "main")
	if err != nil || ahead != 1 || behind != 0 {
		t.Fatalf("after a local commit: got %d/%d, %v; want 1/0, nil", ahead, behind, err)
	}

	// And one on origin that we do not have, putting the two on diverged
	// histories - the case a fast-forward must refuse.
	sb.PeerClone("proj")
	sb.PeerCommit("proj", "remote only")
	if err := gitcmd.Fetch(repo, "origin"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ahead, behind, err = gitcmd.AheadBehind(repo, "origin", "main")
	if err != nil || ahead != 1 || behind != 1 {
		t.Fatalf("diverged: got %d/%d, %v; want 1/1, nil", ahead, behind, err)
	}
}
