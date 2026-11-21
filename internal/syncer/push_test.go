package syncer_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
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
	for _, want := range []string{"tester@peer.example", "BatchMode=yes", "ConnectTimeout=5", "ServerAliveInterval=", "receive 'group/proj'"} {
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

// Two commits a few seconds apart start two background pushes of the same
// branch. The remote takes one and turns the other away with "cannot lock
// ref ... is at X but expected Y", although what it holds now already has the
// loser's commit. That is not a rejection a human has to look at: push again,
// and if the remote has everything, tell the peers as usual.
func TestPushRetriesWhenAnotherPushMovedTheRefMidway(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")

	// The "other push": the commit is already on the remote under another
	// name, and the remote's pre-receive hook moves main onto it the first
	// time it runs, after our push has read main's old value.
	origin := filepath.Join(sb.Home, "remotes", "group/proj.git")
	sb.Git(repo, "push", "-q", "origin", "main:refs/heads/racer")
	hooks := filepath.Join(origin, "hooks")
	sb.Git(origin, "config", "core.hooksPath", hooks)
	testutil.WriteFileIn(t, hooks, "pre-receive", `#!/bin/sh
cat >/dev/null
[ -e raced ] && exit 0
touch raced
env -u GIT_QUARANTINE_PATH git update-ref refs/heads/main refs/heads/racer
`)
	if err := os.Chmod(filepath.Join(hooks, "pre-receive"), 0o755); err != nil {
		t.Fatal(err)
	}

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push returned %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(origin, "raced")); err != nil {
		t.Fatal("the race never happened, so the test proves nothing")
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "")
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

func TestPushRecordsAnUnreachablePeerAsOffline(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(255) // ssh's own "could not connect"
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOffline, "unreachable")
	testutil.AssertNoEvent(t, activity.OpNotify, activity.StatusError)
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

func TestPushNotifiesEveryPeer(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"},
		{Host: "c.local", User: "t"},
		{Host: "d.local", User: "t"},
	}, []string{"group/proj"})
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "sync me")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	calls := sb.SSHCalls()
	for _, host := range []string{"b.local", "c.local", "d.local"} {
		if !strings.Contains(calls, host) {
			t.Errorf("peer %s was never notified; calls:\n%s", host, calls)
		}
	}
	events, _ := activity.Read()
	n := 0
	for _, e := range events {
		if e.Op == activity.OpNotify && e.Status == activity.StatusOK {
			n++
		}
	}
	if n != 3 {
		t.Errorf("got %d ok notify events, want one per peer", n)
	}
}

func TestPushKeepsGoingWhenOnePeerIsUnreachable(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "up-one.local", User: "t"},
		{Host: "down.local", User: "t"},
	}, []string{"group/proj"})
	// Exit 255 only for the host whose name contains "down".
	sb.StubSSHScripted(map[string]string{"*down.local*": "exit 255"}, 0)
	testutil.Commit(t, sb, repo, "sync me")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "up-one.local")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOffline, "down.local")
}

func TestPushNotifiesPeersConcurrently(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"},
		{Host: "c.local", User: "t"},
		{Host: "d.local", User: "t"},
	}, []string{"group/proj"})
	// Each ssh sleeps 300ms. Sequentially that is 900ms+; in parallel it is
	// one sleep. The bound is generous so a slow machine cannot flake it.
	sb.StubSSHScripted(map[string]string{"*": "sleep 0.3"}, 0)
	testutil.Commit(t, sb, repo, "sync me")

	start := time.Now()
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Errorf("notifications took %v; they are not running in parallel", elapsed)
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

func TestPushRecordsAPeerThatCouldNotFetchAsOffline(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(syncer.ExitFetchFailed)
	testutil.Commit(t, sb, repo, "local change")

	syncer.Push("group/proj")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOffline, "could not fetch")
}

// stubRacingPush puts a git on PATH whose every push loses a ref race to
// winner - the way GitHub keeps answering while its advertised ref lags the
// one another overlapping push just wrote. Everything else is real git.
// Returns a func reporting how many pushes were attempted.
func stubRacingPush(t *testing.T, sb *testutil.Sandbox, winner string) func() int {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(sb.Home, "gitbin")
	log := filepath.Join(sb.Home, "pushes.log")
	testutil.WriteFileIn(t, bin, "git", `#!/bin/sh
if [ "$1" = push ]; then
  echo push >> '`+log+`'
  echo " ! [remote rejected] main -> main (cannot lock ref 'refs/heads/main': is at `+winner+` but expected 1111111111111111111111111111111111111111)" >&2
  exit 1
fi
exec '`+realGit+`' "$@"
`)
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "push")
	}
}

// Four commits in three seconds start four overlapping pushes. The one that
// loses the race is told the remote "is at" a commit that already holds its
// own HEAD: nothing is left to deliver, so it is a push, and the peers hear.
func TestALostRefRaceWhoseWinnerHoldsHEADIsAPush(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")
	head := strings.TrimSpace(sb.Git(repo, "rev-parse", "HEAD"))
	pushes := stubRacingPush(t, sb, head)

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push returned %d, want 0", code)
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "")
	if n := pushes(); n != 1 {
		t.Errorf("pushed %d times, want 1: the winner already had everything", n)
	}
}

// A race the remote keeps losing to a commit we do not have is retried a
// bounded number of times, then reported - never looped on forever.
func TestARefRaceToAnUnknownCommitGivesUpAfterAFewTries(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	sb.StubSSH(0)
	testutil.Commit(t, sb, repo, "local change")
	pushes := stubRacingPush(t, sb, "2222222222222222222222222222222222222222")
	defer syncer.SetRefRaceBackoff(0)()

	syncer.Push("group/proj")
	testutil.AssertEvent(t, activity.OpPush, activity.StatusError, "cannot lock ref")
	if n := pushes(); n < 3 || n > 6 {
		t.Errorf("pushed %d times, want a few retries", n)
	}
}
