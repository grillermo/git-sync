package syncer_test

import (
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestAnnounceCatchesUpFromTheRemoteAndAsksEveryPeerToRetry(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"},
	}, []string{"group/proj"})
	sb.StubSSH(0)
	// Committed elsewhere while this machine was off.
	sb.PeerClone("group/proj")
	sb.PeerCommit("group/proj", "made while we were off")

	if code := syncer.Announce(); code != 0 {
		t.Fatalf("Announce = %d", code)
	}
	if out := sb.Git(repo, "log", "--oneline", "-1"); !strings.Contains(out, "made while we were off") {
		t.Errorf("did not catch up from the remote:\n%s", out)
	}
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusOK, "fast-forward")
	calls := sb.SSHCalls()
	for _, host := range []string{"b.local", "c.local"} {
		if !strings.Contains(calls, "t@"+host+" ~/.gitsync/bin/git-sync retry") {
			t.Errorf("%s was not asked to retry:\n%s", host, calls)
		}
	}
}

func TestAnnounceSendsThisMachinesOwnBacklog(t *testing.T) {
	sb := testutil.NewSandbox(t)
	a, _ := twoRepoMesh(t, sb, "laptop.local")
	sb.StubSSH(255)
	testutil.Commit(t, sb, a, "one")
	syncer.Push("group/a")

	sb.StubSSH(0)
	syncer.Announce()
	if n := countCalls(sb, "laptop.local", "group/a"); n != 2 {
		t.Errorf("backlog not sent: %d notifies\n%s", n, sb.SSHCalls())
	}
	if syncer.HasPending() {
		t.Error("backlog left after announce")
	}
}

func TestAnnounceKeepsTryingAPeerThatIsDownThenGivesUpQuietly(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "off.local", User: "t"}}, []string{"group/proj"})
	sb.StubSSH(255)
	t.Setenv("GITSYNC_ANNOUNCE_TIMEOUT", "2500ms")

	start := time.Now()
	if code := syncer.Announce(); code != 0 {
		t.Fatalf("an offline peer is not a failure: Announce = %d", code)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Announce ran %v past its timeout", took)
	}
	if n := strings.Count(sb.SSHCalls(), "git-sync retry"); n < 2 {
		t.Errorf("tried the peer %d times, want retries with backoff", n)
	}
	// Presumably off; it announces itself when it starts.
	testutil.AssertNoEvent(t, activity.OpReceive, activity.StatusError)
}

func TestAnnounceRetriesARemoteThatIsNotReachableYet(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithPeers(t, sb, nil, []string{"group/proj"})
	origin := strings.TrimSpace(sb.Git(repo, "remote", "get-url", "origin"))
	sb.Git(repo, "remote", "set-url", "origin", "/nonexistent/proj.git")
	t.Setenv("GITSYNC_ANNOUNCE_TIMEOUT", "1500ms")

	// The network comes up partway through.
	go func() {
		time.Sleep(500 * time.Millisecond)
		sb.Git(repo, "remote", "set-url", "origin", origin)
	}()
	syncer.Announce()
	testutil.AssertEvent(t, activity.OpReceive, activity.StatusError, "fetch")
	testutil.AssertNoEvent(t, activity.OpReceive, activity.StatusWarn)
	events, _ := activity.Read()
	if last := events[len(events)-1]; last.Status == activity.StatusError {
		t.Errorf("never caught up once the remote was back: %+v", last)
	}
}
