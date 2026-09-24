package setup_test

import (
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

// TestCheckPeersAsksEveryMachine confirms CheckPeers asks every peer in one
// round trip each and keys the answers by host, so a caller can render one
// machine's mismatches without the others' answers shifting in.
func TestCheckPeersAsksEveryMachine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"git-sync-repo-check": "url group/proj git@example.com:me/proj.git\n",
	}, 0)
	peers := []setup.PeerTarget{
		{Peer: config.Peer{Host: "b.local", User: "t"}, BaseDir: "/home/t/code"},
		{Peer: config.Peer{Host: "c.local", User: "t"}, BaseDir: "/home/t/code"},
	}
	want := []setup.RepoWant{{Rel: "group/proj", RemoteURL: "git@example.com:me/proj.git"}}

	got := setup.CheckPeers(peers, want, config.DefaultRemoteNames)
	if len(got) != 2 {
		t.Fatalf("got answers from %d machines, want 2: %+v", len(got), got)
	}
	for _, host := range []string{"b.local", "c.local"} {
		checks, ok := got[host]
		if !ok || len(checks) != 1 {
			t.Fatalf("no answer for %s: %+v", host, got)
		}
		if checks[0].State != setup.RepoPresent {
			t.Errorf("%s: state = %q, want present", host, checks[0].State)
		}
	}
}

// TestCheckPeersMarksAnUnreachablePeerUnchecked confirms an unreachable
// machine still gets a row per repo, marked unchecked, rather than vanishing
// from the map entirely - so it renders as "we do not know" instead of
// silently looking fine.
func TestCheckPeersMarksAnUnreachablePeerUnchecked(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(nil, 255)
	peers := []setup.PeerTarget{
		{Peer: config.Peer{Host: "down.local", User: "t"}, BaseDir: "/home/t/code"},
	}
	want := []setup.RepoWant{{Rel: "group/proj", RemoteURL: "git@example.com:me/proj.git"}}

	got := setup.CheckPeers(peers, want, config.DefaultRemoteNames)
	checks, ok := got["down.local"]
	if !ok || len(checks) != 1 {
		t.Fatalf("no answer for down.local: %+v", got)
	}
	if checks[0].State != setup.RepoUnchecked {
		t.Errorf("state = %q, want unchecked", checks[0].State)
	}
}
