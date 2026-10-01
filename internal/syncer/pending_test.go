package syncer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

// countCalls counts ssh invocations naming both host and the receive of rel.
func countCalls(sb *testutil.Sandbox, host, rel string) int {
	n := 0
	for _, ln := range strings.Split(sb.SSHCalls(), "\n") {
		if strings.Contains(ln, host) && strings.Contains(ln, "receive '"+rel+"'") {
			n++
		}
	}
	return n
}

func pendingEntries(t *testing.T, sb *testutil.Sandbox, parts ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(append([]string{sb.GitsyncHome, "pending"}, parts...)...))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func twoRepoMesh(t *testing.T, sb *testutil.Sandbox, peers ...string) (a, b string) {
	t.Helper()
	a, b = sb.MakeRepo("group/a"), sb.MakeRepo("group/b")
	var ps []config.Peer
	for _, h := range peers {
		ps = append(ps, config.Peer{Host: h, User: "t"})
	}
	testutil.SaveConfigWithPeers(t, sb, ps, []string{"group/a", "group/b"})
	return a, b
}

func TestAnUnreachablePeerIsNotifiedOnTheNextPushOfAnyRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, b := twoRepoMesh(t, sb, "up.local", "laptop.local")

	sb.StubSSHScripted(map[string]string{"*laptop.local*": "exit 255"}, 0)
	testutil.Commit(t, sb, a, "made while the laptop was off")
	syncer.Push("group/a")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusError, "will retry")
	if got := pendingEntries(t, sb, "notify", "laptop.local"); len(got) != 1 {
		t.Fatalf("missed notify not queued: %v", got)
	}

	// The laptop is back; a commit in a different repo brings it up to date
	// on both.
	sb.StubSSH(0)
	testutil.Commit(t, sb, b, "unrelated")
	syncer.Push("group/b")

	if n := countCalls(sb, "laptop.local", "group/a"); n != 2 {
		t.Errorf("laptop told about group/a %d times, want 2 (missed, then retried)\n%s", n, sb.SSHCalls())
	}
	if n := countCalls(sb, "up.local", "group/a"); n != 1 {
		t.Errorf("a peer that already had group/a was told again (%d times)", n)
	}
	if got := pendingEntries(t, sb, "notify", "laptop.local"); len(got) != 0 {
		t.Errorf("delivered notify still queued: %v", got)
	}
}

func TestAPeerThatIsStillDownIsNotRetriedInTheSameRun(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, b := twoRepoMesh(t, sb, "laptop.local")
	sb.StubSSH(255)

	testutil.Commit(t, sb, a, "one")
	syncer.Push("group/a")
	testutil.Commit(t, sb, b, "two")
	syncer.Push("group/b")

	// The second run found the laptop down for group/b, so it did not spend
	// another ConnectTimeout on its backlog.
	if n := countCalls(sb, "laptop.local", "group/a"); n != 1 {
		t.Errorf("group/a sent to a down peer %d times, want 1", n)
	}
	if got := pendingEntries(t, sb, "notify", "laptop.local"); len(got) != 2 {
		t.Errorf("want both repos queued, got %v", got)
	}
}

func TestAPeerThatCouldNotFetchIsRetried(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, _ := twoRepoMesh(t, sb, "laptop.local")
	sb.StubSSH(syncer.ExitFetchFailed)

	testutil.Commit(t, sb, a, "one")
	syncer.Push("group/a")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusError, "could not fetch")
	if got := pendingEntries(t, sb, "notify", "laptop.local"); len(got) != 1 {
		t.Errorf("fetch failure not queued: %v", got)
	}
}

func TestAReceiveFailureIsNotRetried(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, _ := twoRepoMesh(t, sb, "laptop.local")
	sb.StubSSH(1)

	testutil.Commit(t, sb, a, "one")
	syncer.Push("group/a")
	if got := pendingEntries(t, sb, "notify", "laptop.local"); len(got) != 0 {
		t.Errorf("a non-connectivity failure was queued: %v", got)
	}
}

func TestAPushMadeOfflineIsPushedByTheNextRun(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, b := twoRepoMesh(t, sb, "peer.local")
	sb.StubSSH(0)

	origin := strings.TrimSpace(sb.Git(a, "remote", "get-url", "origin"))
	// Nothing listens on port 1: git fails with "Connection refused".
	sb.Git(a, "remote", "set-url", "origin", "http://127.0.0.1:1/a.git")
	testutil.Commit(t, sb, a, "made offline")
	syncer.Push("group/a")
	testutil.AssertEvent(t, activity.OpPush, activity.StatusError, "will retry")
	if got := pendingEntries(t, sb, "push"); len(got) != 1 {
		t.Fatalf("offline push not queued: %v", got)
	}
	if sb.SSHCalls() != "" {
		t.Error("notified peers about a push that never happened")
	}

	// Back online.
	sb.Git(a, "remote", "set-url", "origin", origin)
	testutil.Commit(t, sb, b, "unrelated")
	syncer.Push("group/b")

	if out := sb.Git(a, "log", "--oneline", "origin/main"); !strings.Contains(out, "made offline") {
		t.Errorf("queued push never reached the remote:\n%s", out)
	}
	if n := countCalls(sb, "peer.local", "group/a"); n != 1 {
		t.Errorf("peer told about the retried push %d times, want 1", n)
	}
	if got := pendingEntries(t, sb, "push"); len(got) != 0 {
		t.Errorf("pushed repo still queued: %v", got)
	}
}

func TestARejectedPushIsNotQueued(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, _ := twoRepoMesh(t, sb, "peer.local")
	sb.StubSSH(0)
	sb.Git(a, "remote", "set-url", "origin", filepath.Join(sb.Home, "gone.git"))
	testutil.Commit(t, sb, a, "one")

	syncer.Push("group/a")
	if got := pendingEntries(t, sb, "push"); len(got) != 0 {
		t.Errorf("a push that failed for a non-network reason was queued: %v", got)
	}
}

func TestRetryDeliversTheBacklog(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, _ := twoRepoMesh(t, sb, "laptop.local")
	sb.StubSSH(255)
	testutil.Commit(t, sb, a, "one")
	syncer.Push("group/a")

	if !syncer.HasPending() {
		t.Fatal("HasPending = false with a queued notify")
	}
	sb.StubSSH(0)
	if code := syncer.Retry(); code != 0 {
		t.Fatalf("Retry = %d", code)
	}
	if n := countCalls(sb, "laptop.local", "group/a"); n != 2 {
		t.Errorf("Retry did not re-notify: %d calls", n)
	}
	if syncer.HasPending() {
		t.Error("HasPending = true after a successful retry")
	}
}

func TestRetryDropsARepoNoLongerSelected(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, _ := twoRepoMesh(t, sb, "laptop.local")
	sb.StubSSH(255)
	testutil.Commit(t, sb, a, "one")
	syncer.Push("group/a")

	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "laptop.local", User: "t"}}, []string{"group/b"})
	sb.StubSSH(0)
	syncer.Retry()
	if n := countCalls(sb, "laptop.local", "group/a"); n != 1 {
		t.Errorf("retried a deselected repo")
	}
	if syncer.HasPending() {
		t.Error("deselected repo left in the queue")
	}
}
