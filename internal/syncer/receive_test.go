package syncer_test

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/lock"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestReceiveFastForwardsACleanTree(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from-peer")

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	if out := sb.Git(repo, "log", "--oneline"); !strings.Contains(out, "from-peer") {
		t.Errorf("did not fast-forward:\n%s", out)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusOK, "fast-forward")
}

func TestReceiveIsAHarmlessNoOpWhenUpToDate(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	if out := sb.Git(repo, "status", "--porcelain"); out != "" {
		t.Errorf("working tree should be untouched, got:\n%s", out)
	}
	testutil.AssertNoEvent(t, activity.OpReceive, activity.StatusWarn)
}

func TestReceiveStashesFastForwardsAndRestores(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from-peer")
	sb.Dirty(repo)

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	if out := sb.Git(repo, "log", "--oneline"); !strings.Contains(out, "from-peer") {
		t.Error("should have fast-forwarded")
	}
	// Both the tracked edit and the untracked file must be back.
	testutil.AssertFileContains(t, filepath.Join(repo, "NOTES.md"), "work in progress")
	testutil.AssertFileContains(t, filepath.Join(repo, "README.md"), "uncommitted edit")
	if out := sb.Git(repo, "stash", "list"); out != "" {
		t.Errorf("nothing should be left in the stash, got:\n%s", out)
	}
}

func TestReceiveRestoresTheStashEvenWhenHistoryDiverged(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from-peer")

	// Diverge locally, then dirty the tree.
	testutil.WriteFileIn(t, repo, "OTHER.md", "local\n")
	sb.Git(repo, "add", "-A")
	sb.Git(repo, "commit", "-qm", "divergent local commit")
	sb.Dirty(repo)

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusWarn, "diverged")

	// The fetch still happened, so the user can merge by hand.
	if out := sb.Git(repo, "log", "--oneline", "origin/main"); !strings.Contains(out, "from-peer") {
		t.Error("should still have fetched")
	}
	// Local history untouched.
	if out := sb.Git(repo, "log", "--oneline", "-1"); !strings.Contains(out, "divergent local commit") {
		t.Error("local history should be untouched")
	}
	// The assertion that matters most: the dirty tree came back.
	testutil.AssertFileContains(t, filepath.Join(repo, "NOTES.md"), "work in progress")
	if out := sb.Git(repo, "stash", "list"); out != "" {
		t.Errorf("a diverged sync must still restore the stash, got:\n%s", out)
	}
}

func TestReceiveLeavesAConflictingStashInPlace(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	peer := sb.PeerClone("group/proj")
	// Deliberately edits README.md, unlike PeerCommit, so it lands on the
	// exact tracked file the local dirty edit below also touches.
	testutil.Commit(t, sb, peer, "peer edits README")
	sb.Git(peer, "push", "-q")
	// Edit the same file the peer changed, so the pop must conflict.
	testutil.AppendFileIn(t, repo, "README.md", "local conflicting edit\n")

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusWarn, "stash pop conflicted")
	// Never auto-resolve: the entry is preserved for the user.
	if out := sb.Git(repo, "stash", "list"); out == "" {
		t.Error("a conflicting stash entry must be preserved, not dropped")
	}
}

func TestReceiveSkipsARepoNotOnThisMachine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	if code := syncer.Receive("group/ghost"); code != syncer.ExitRepoNotHere {
		t.Errorf("Receive = %d, want %d so ssh carries it back to the pusher",
			code, syncer.ExitRepoNotHere)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusSkip, "not on this machine")
}

func TestReceiveDeclinesAnUnselectedRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj") // present on disk, but not ticked here
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"other/thing"})

	if code := syncer.Receive("group/proj"); code != syncer.ExitRepoNotHere {
		t.Errorf("Receive = %d, want %d so the pusher records a harmless skip",
			code, syncer.ExitRepoNotHere)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusSkip, "not selected")
}

func TestReceiveSkipsAPathThatIsNotARepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	testutil.MkdirAll(t, filepath.Join(sb.BaseDir, "notarepo"))

	if code := syncer.Receive("notarepo"); code != syncer.ExitRepoNotHere {
		t.Errorf("Receive = %d, want %d", code, syncer.ExitRepoNotHere)
	}
}

func TestReceiveSyncsAWorktreeWhereDotGitIsAFile(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	wt := filepath.Join(sb.BaseDir, "group", "proj-wt")
	sb.Git(repo, "worktree", "add", "-q", wt, "-b", "wt-branch")
	// After the worktree exists, so it is discovered and selected too.
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	// Must not be mistaken for a missing repo just because .git is a file.
	if code := syncer.Receive("group/proj-wt"); code == syncer.ExitRepoNotHere {
		t.Error("a linked worktree is a real repo")
	}
}

func TestReceiveSkipsDetachedHead(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.Git(repo, "checkout", "-q", "--detach")

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Errorf("Receive = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusSkip, "detached HEAD")
}

func TestReceiveSkipsABranchTheRemoteDoesNotHave(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.Git(repo, "checkout", "-q", "-b", "orphan")

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Errorf("Receive = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusSkip, "not on the remote")
	testutil.AssertNoEvent(t, activity.OpReceive, activity.StatusWarn)
}

func TestReceivePullsFromTheSameRemotePushUsed(t *testing.T) {
	// The two machines meet at the remote; both sides must resolve its name
	// the same way or the sync quietly does nothing.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepoNamedRemote("group/proj", "github")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from-peer")

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	if out := sb.Git(repo, "log", "--oneline"); !strings.Contains(out, "from-peer") {
		t.Errorf("did not fast-forward onto github/main:\n%s", out)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusOK, "github")
}

func TestReceiveIgnoresAnUpstreamPointingElsewhere(t *testing.T) {
	// The branch tracks origin, but the shared remote is github. Following
	// @{upstream} here would fetch the wrong repository.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj") // main tracks origin/main
	sb.AddRemote(t, repo, "github", "group/proj-gh")
	sb.Git(repo, "push", "-q", "github", "main")
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	// The peer commits through github, which is what push would have used.
	// AddRemote(repo, "github", "group/proj-gh") names its bare repo
	// "group/proj-gh-github.git", so that's what PeerClone must target.
	gh := sb.PeerClone("group/proj-gh-github")
	testutil.Commit(t, sb, gh, "from-peer-via-github")
	sb.Git(gh, "push", "-q")

	if code := syncer.Receive("group/proj"); code != 0 {
		t.Fatalf("Receive = %d, want 0", code)
	}
	if out := sb.Git(repo, "log", "--oneline"); !strings.Contains(out, "from-peer-via-github") {
		t.Errorf("receive followed the wrong remote:\n%s", out)
	}
}

func TestReceiveSkipsARepoWithNoSharedRemote(t *testing.T) {
	sb := testutil.NewSandbox(t)
	local := filepath.Join(sb.BaseDir, "local-only")
	testutil.MkdirAll(t, local)
	sb.Git(sb.Home, "init", "-q", local)
	testutil.Commit(t, sb, local, "initial")
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"local-only"})

	if code := syncer.Receive("local-only"); code != 0 {
		t.Errorf("Receive = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusWarn, "no remote")
}

func TestReceiveGivesUpWhenAnotherRunHoldsTheLock(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	held, err := lock.Acquire("group/proj", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	t.Setenv("GITSYNC_LOCK_TIMEOUT", "200ms")
	if code := syncer.Receive("group/proj"); code != 0 {
		t.Errorf("giving up on a busy lock is not a failure: got %d", code)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusSkip, "already in progress")
}

func TestReceiveConcurrentRunsNeverLoseADirtyTree(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "from-peer-1")
	sb.PeerCommit("group/proj", "from-peer-2")
	sb.Dirty(repo)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); syncer.Receive("group/proj") }()
	}
	wg.Wait()

	// Whichever run won, the outcome is the same.
	if out := sb.Git(repo, "log", "--oneline"); !strings.Contains(out, "from-peer-2") {
		t.Error("should have caught up")
	}
	testutil.AssertFileContains(t, filepath.Join(repo, "NOTES.md"), "work in progress")
	if out := sb.Git(repo, "stash", "list"); out != "" {
		t.Errorf("nothing stranded in the stash, got:\n%s", out)
	}
}
