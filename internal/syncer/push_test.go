package syncer_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestPushSendsCommitsAndNotifiesThePeer(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)

	testutil.Commit(t, sb, repo, "local change")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push returned %d, want 0", code)
	}

	if out := sb.Git(repo, "log", "--oneline", "origin/main"); !strings.Contains(out, "local change") {
		t.Errorf("commit did not reach origin:\n%s", out)
	}
	calls := sb.SSHCalls()
	for _, want := range []string{"tester@peer.example", "BatchMode=yes", "ConnectTimeout=5", "receive 'group/proj'"} {
		if !strings.Contains(calls, want) {
			t.Errorf("ssh call %q missing %q", calls, want)
		}
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "")
}

func TestPushDoesNotNotifyWhenThePushFails(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	sb.Git(repo, "remote", "set-url", "origin", filepath.Join(sb.Home, "gone.git"))
	testutil.Commit(t, sb, repo, "local change")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Errorf("a failed push is logged, not a crash: got %d", code)
	}
	if sb.SSHCalls() != "" {
		t.Error("must not notify the peer after a failed push")
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusError, "")
}

func TestPushRecordsAPeerWithoutTheRepoAsASkip(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(3) // receive's "not on this machine" exit code
	testutil.Commit(t, sb, repo, "local change")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Errorf("a peer without the repo is not a failure: got %d", code)
	}
	// The push still stands.
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "")
	// And the notify is a skip, not a success and not an error.
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusSkip, "no copy")
}

func TestPushRecordsAnUnreachablePeerAsAnError(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(255) // ssh's own "could not connect"
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusError, "unreachable")
}

func TestPushDistinguishesAReceiveFailure(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(1)
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusError, "receive failed")
	testutil.AssertNoEvent(t, activity.OpNotify, activity.StatusSkip)
}

func TestPushUsesTheGithubRemoteWhenThereIsOne(t *testing.T) {
	// The remote is the transport. A repo whose shared remote is called
	// github must not be pushed to origin instead.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepoNamedRemote("group/proj", "github")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	if out := sb.Git(repo, "log", "--oneline", "github/main"); !strings.Contains(out, "local change") {
		t.Errorf("commit did not reach github/main:\n%s", out)
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "github")
}

func TestPushPrefersGithubOverOrigin(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj") // has origin
	other := sb.AddRemote(t, repo, "github", "group/proj-gh")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")

	if out := sb.Git(other, "log", "--oneline", "main"); !strings.Contains(out, "local change") {
		t.Errorf("github should have won the tie:\n%s", out)
	}
	if out := sb.Git(repo, "log", "--oneline", "origin/main"); strings.Contains(out, "local change") {
		t.Error("must push to exactly one remote, not both")
	}
}

func TestPushSkipsARepoWithNoSharedRemote(t *testing.T) {
	// Nothing to sync through, and nothing ssh could tell the peer to pull.
	sb := testutil.NewSandbox(t)
	local := filepath.Join(sb.BaseDir, "local-only")
	testutil.MkdirAll(t, local)
	sb.Git(sb.Home, "init", "-q", local)
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"local-only"})
	sb.StubSSH(0)
	testutil.Commit(t, sb, local, "local change")

	if code := syncer.Push("local-only"); code != 0 {
		t.Errorf("Push = %d, want 0: a repo with no remote is not a crash", code)
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusWarn, "no remote")
	if sb.SSHCalls() != "" {
		t.Error("must not notify the peer about a repo that cannot sync")
	}
}

func TestPushHonoursAConfiguredRemoteName(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepoNamedRemote("group/proj", "gitlab")
	testutil.SaveConfigWithRemotes(t, sb, "peer.example", "tester", []string{"gitlab"})
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "gitlab")
}

func TestPushSkipsDetachedHead(t *testing.T) {
	// There is no branch to name on the remote.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	sb.Git(repo, "checkout", "-q", "--detach")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Errorf("Push = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusSkip, "detached HEAD")
	if sb.SSHCalls() != "" {
		t.Error("nothing was pushed, so there is nothing for the peer to pull")
	}
}

func TestPushRejectsAnEscapingRelpath(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	if code := syncer.Push("../../etc"); code == 0 {
		t.Error("Push should reject a relpath that escapes base_dir")
	}
	if sb.SSHCalls() != "" {
		t.Error("must not ssh with an escaping relpath")
	}
}

func TestPushRejectsAQuoteInTheRelpath(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	// The relpath is interpolated into a single-quoted remote command.
	if code := syncer.Push("we'ird"); code == 0 {
		t.Error("Push should reject a single quote in the relpath")
	}
	if sb.SSHCalls() != "" {
		t.Error("must not ssh with an unquotable relpath")
	}
}
