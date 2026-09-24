package setup_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

// installLoopbackSSH puts a fake ssh on PATH that strips ssh's -o flags and
// the user@host target, then runs the remaining remote command through a real
// shell. The sandbox has already pointed GIT_CONFIG_GLOBAL and friends at the
// temp tree for this process, and the stub's children inherit that, so every
// peer's repos are driven by the same sandboxed git as ours.
func installLoopbackSSH(t *testing.T, sb *testutil.Sandbox) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"while [ \"$1\" = \"-o\" ]; do shift 2; done\n" +
		"shift\n" +
		"sh -c \"$1\"\n"
	bin := filepath.Join(sb.Home, "bin")
	testutil.MkdirAll(t, bin)
	path := filepath.Join(bin, "ssh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write ssh stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// syncFixtureN sets up one repo present on this machine and on
// len(peerNames) other machines, each simulated with its own real clone
// under sb.Home and reached through the loopback ssh trick above - so
// ApplySync's remote steps run real git, not a stub, for every peer in the
// mesh at once. Returns the config to measure with, the peer targets in the
// same order as peerNames, and each peer's clone path in that same order.
func syncFixtureN(t *testing.T, sb *testutil.Sandbox, rel string, peerNames ...string) (config.Config, []setup.PeerTarget, []string) {
	t.Helper()
	sb.MakeRepo(rel)
	installLoopbackSSH(t, sb)

	var cfgPeers []config.Peer
	var targets []setup.PeerTarget
	var clones []string
	for _, name := range peerNames {
		peerHome := filepath.Join(sb.Home, "peer-"+name)
		dst := filepath.Join(peerHome, rel)
		testutil.MkdirAll(t, filepath.Dir(dst))
		sb.Git(sb.Home, "clone", "-q", filepath.Join(sb.Home, "remotes", rel+".git"), dst)
		p := config.Peer{Host: name, User: "t"}
		cfgPeers = append(cfgPeers, p)
		targets = append(targets, setup.PeerTarget{Peer: p, BaseDir: peerHome})
		clones = append(clones, dst)
	}
	cfg := config.Config{BaseDir: sb.BaseDir, Peers: cfgPeers}
	return cfg, targets, clones
}

// commitNoPush makes a commit in dir and deliberately leaves it unpushed -
// the state that caused a real sync to silently stop converging.
func commitNoPush(t *testing.T, sb *testutil.Sandbox, dir, msg string) {
	t.Helper()
	testutil.AppendFileIn(t, dir, "LOCAL.md", msg+"\n")
	sb.Git(dir, "add", "-A")
	sb.Git(dir, "commit", "-qm", msg)
}

func measureAll(t *testing.T, cfg config.Config, peers []setup.PeerTarget, repos ...string) []setup.RepoSync {
	t.Helper()
	got, err := setup.MeasureSync(cfg, peers, repos)
	if err != nil {
		t.Fatalf("MeasureSync: %v", err)
	}
	return got
}

// posFor picks out one peer's measured position by host, failing the test if
// it is not present - every repo must be measured on every peer.
func posFor(t *testing.T, r setup.RepoSync, host string) setup.SyncPos {
	t.Helper()
	for _, p := range r.There {
		if p.Peer.Host == host {
			return p.Pos
		}
	}
	t.Fatalf("no measured position for %s in %+v", host, r)
	return setup.SyncPos{}
}

func TestMeasureSyncSeesEveryMachineLevel(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, _ := syncFixtureN(t, sb, "proj", "b.local", "c.local")

	got := measureAll(t, cfg, peers, "proj")
	if len(got) != 1 {
		t.Fatalf("got %d repos, want 1", len(got))
	}
	if got[0].Here.Ahead != 0 || got[0].Here.Behind != 0 {
		t.Errorf("here = %+v, want level", got[0].Here)
	}
	if len(got[0].There) != 2 {
		t.Fatalf("measured on %d peers, want 2", len(got[0].There))
	}
	for _, p := range got[0].There {
		if p.Pos.Ahead != 0 || p.Pos.Behind != 0 {
			t.Errorf("%s = %+v, want level", p.Peer.Host, p.Pos)
		}
	}

	var out bytes.Buffer
	if setup.RenderSyncPlan(&out, got) {
		t.Errorf("RenderSyncPlan reported work to do:\n%s", out.String())
	}
}

// The regression test for the incident this stage exists to prevent: a peer
// held a commit it had never pushed, so every later receive refused the
// fast-forward and warned, forever, with nothing retrying it. Now generalised
// to a second peer that has nothing local of its own, to confirm it also
// lands what the first peer published rather than being left behind.
func TestInitialSyncPushesAnUnpushedPeerCommit(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, clones := syncFixtureN(t, sb, "proj", "b.local", "c.local")
	commitNoPush(t, sb, clones[0], "b worked offline")

	before := measureAll(t, cfg, peers, "proj")
	if posFor(t, before[0], "b.local").Ahead != 1 {
		t.Fatalf("b.local ahead = %d, want 1", posFor(t, before[0], "b.local").Ahead)
	}
	if before[0].Here.Behind != 0 {
		t.Fatalf("here behind = %d, want 0 (the commit is not on the remote yet)", before[0].Here.Behind)
	}

	after := setup.ApplySync(cfg, peers, before)

	for _, p := range after[0].There {
		if p.Pos.Ahead != 0 || p.Pos.Behind != 0 {
			t.Errorf("%s = %+v, want level after the initial sync", p.Peer.Host, p.Pos)
		}
	}
	if after[0].Here.Ahead != 0 || after[0].Here.Behind != 0 {
		t.Errorf("here = %+v, want level after the initial sync", after[0].Here)
	}
	if log := sb.Git(sb.BaseDir+"/proj", "log", "--oneline"); !strings.Contains(log, "b worked offline") {
		t.Errorf("this machine never got b.local's commit:\n%s", log)
	}
	if log := sb.Git(clones[1], "log", "--oneline"); !strings.Contains(log, "b worked offline") {
		t.Errorf("c.local never got b.local's commit:\n%s", log)
	}

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, after); n != 0 {
		t.Errorf("RenderSyncResult = %d unresolved, want 0:\n%s", n, out.String())
	}
}

func TestInitialSyncPushesOurCommitAndEveryPeerLandsIt(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, clones := syncFixtureN(t, sb, "proj", "b.local", "c.local")
	commitNoPush(t, sb, sb.BaseDir+"/proj", "committed before install")

	after := setup.ApplySync(cfg, peers, measureAll(t, cfg, peers, "proj"))

	if after[0].Here.Ahead != 0 {
		t.Errorf("here = %+v, want level", after[0].Here)
	}
	for _, p := range after[0].There {
		if p.Pos.Ahead != 0 || p.Pos.Behind != 0 {
			t.Errorf("%s = %+v, want level", p.Peer.Host, p.Pos)
		}
	}
	for i, clone := range clones {
		if log := sb.Git(clone, "log", "--oneline"); !strings.Contains(log, "committed before install") {
			t.Errorf("peer %d (%s) never got our commit:\n%s", i, peers[i].Peer.Host, log)
		}
	}
}

// Push and fast-forward cannot reconcile two machines that each have commits
// the other lacks. git-sync must say so rather than merge: merging is the
// user's call, and a merge commit here would be one nobody asked for. A
// second, healthy peer must still land normally - one diverged machine must
// not stall the rest of the mesh.
func TestInitialSyncLeavesDivergedHistoryForTheUser(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, clones := syncFixtureN(t, sb, "proj", "b.local", "c.local")
	commitNoPush(t, sb, sb.BaseDir+"/proj", "ours")
	commitNoPush(t, sb, clones[0], "b's own")
	// Getting our side onto the remote is what makes b.local diverged.
	sb.Git(sb.BaseDir+"/proj", "push", "-q", "origin", "main")

	got := measureAll(t, cfg, peers, "proj")
	b := posFor(t, got[0], "b.local")
	if b.Ahead == 0 || b.Behind == 0 {
		t.Fatalf("b.local = %+v, want it diverged (both ahead and behind)", b)
	}
	c := posFor(t, got[0], "c.local")
	if c.Behind == 0 || c.Ahead != 0 {
		t.Fatalf("c.local = %+v, want it purely behind", c)
	}

	after := setup.ApplySync(cfg, peers, got)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, after); n != 1 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 1:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "diverged") {
		t.Errorf("result does not explain the divergence:\n%s", out.String())
	}
	if log := sb.Git(clones[0], "log", "--oneline"); strings.Contains(log, "Merge") {
		t.Errorf("initial sync created a merge commit on b.local:\n%s", log)
	}
	if log := sb.Git(clones[0], "log", "--oneline"); !strings.Contains(log, "b's own") {
		t.Errorf("b.local lost its own commit:\n%s", log)
	}
	if log := sb.Git(clones[1], "log", "--oneline"); !strings.Contains(log, "ours") {
		t.Errorf("c.local did not fast-forward onto our commit:\n%s", log)
	}
}

// The mesh only ever meets on the same branch, so a mismatch is reported
// rather than quietly half-synced - and it blocks the whole repo, not just
// the one machine that disagrees, since nothing we are willing to do changes
// which branch a machine is on.
func TestInitialSyncReportsDifferentBranches(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, clones := syncFixtureN(t, sb, "proj", "b.local", "c.local")
	// b.local is pushed and perfectly level - on a branch we are not on.
	// Both sides look healthy in isolation; only the comparison shows it.
	sb.Git(clones[0], "checkout", "-q", "-b", "other")
	sb.Git(clones[0], "push", "-q", "-u", "origin", "other")

	got := measureAll(t, cfg, peers, "proj")
	b := posFor(t, got[0], "b.local")
	if got[0].Here.Branch != "main" || b.Branch != "other" {
		t.Fatalf("branches = %q / %q, want main / other", got[0].Here.Branch, b.Branch)
	}
	if b.Err != "" {
		t.Fatalf("b.local position unexpectedly errored: %q", b.Err)
	}

	var out bytes.Buffer
	if !setup.RenderSyncPlan(&out, got) {
		t.Fatalf("plan reported nothing to do:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "different branches") {
		t.Errorf("plan does not name the branch mismatch:\n%s", out.String())
	}

	after := setup.ApplySync(cfg, peers, got)
	if n := setup.RenderSyncResult(&out, after); n != 1 {
		t.Errorf("RenderSyncResult = %d unresolved, want 1", n)
	}
}

// A repo no peer has cloned must not be reported as fine, and must not stop
// the other repos from being levelled.
func TestInitialSyncMarksARepoNoPeerHas(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, _ := syncFixtureN(t, sb, "proj", "b.local", "c.local")
	sb.MakeRepo("solo")

	got := measureAll(t, cfg, peers, "proj", "solo")
	if len(got) != 2 {
		t.Fatalf("got %d repos, want 2", len(got))
	}
	solo := got[1]
	if len(solo.There) != 2 {
		t.Fatalf("solo measured on %d peers, want 2", len(solo.There))
	}
	for _, p := range solo.There {
		if p.Pos.Err == "" {
			t.Errorf("%s position for solo = %+v, want an error: neither peer has this repo", p.Peer.Host, p.Pos)
		}
	}

	var out bytes.Buffer
	setup.RenderSyncPlan(&out, got)
	if !strings.Contains(out.String(), "solo") {
		t.Errorf("plan does not mention the one-sided repo:\n%s", out.String())
	}
}

// A dirty working tree on one peer must not be the reason a repo is left
// unlevelled: the steady-state receive stashes around the fast-forward, so
// the initial sync has to as well - and a second, clean peer must still land
// normally alongside it.
func TestInitialSyncLevelsARepoWithADirtyTreeOnOnePeer(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, clones := syncFixtureN(t, sb, "proj", "b.local", "c.local")

	// We are ahead, and b.local has an unrelated uncommitted edit.
	commitNoPush(t, sb, sb.BaseDir+"/proj", "new work")
	sb.Git(sb.BaseDir+"/proj", "push", "-q", "origin", "main")
	testutil.AppendFileIn(t, clones[0], "SCRATCH.md", "work in progress\n")

	before := measureAll(t, cfg, peers, "proj")
	if posFor(t, before[0], "b.local").Behind == 0 {
		t.Fatalf("b.local = %+v, want it behind", posFor(t, before[0], "b.local"))
	}

	after := setup.ApplySync(cfg, peers, before)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, after); n != 0 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 0:\n%s", n, out.String())
	}
	if log := sb.Git(clones[0], "log", "--oneline"); !strings.Contains(log, "new work") {
		t.Errorf("b.local did not fast-forward:\n%s", log)
	}
	// The stash must have come back.
	if b, err := os.ReadFile(filepath.Join(clones[0], "SCRATCH.md")); err != nil ||
		!strings.Contains(string(b), "work in progress") {
		t.Errorf("uncommitted work was not restored on b.local: %v", err)
	}
	// c.local, with nothing local, must land too - not just the machine that
	// happened to be dirty.
	if log := sb.Git(clones[1], "log", "--oneline"); !strings.Contains(log, "new work") {
		t.Errorf("c.local did not fast-forward:\n%s", log)
	}
}

// Two peers ahead of the same repo is not blocked: exactly one - the first
// in peer order - publishes, and the other is left alone rather than merged.
// Real git on two real peer clones, driven through ApplySync end to end.
func TestApplySyncLeavesASecondAheadPeerAloneForTheUser(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peers, clones := syncFixtureN(t, sb, "proj", "b.local", "c.local")
	commitNoPush(t, sb, clones[0], "b worked offline")
	commitNoPush(t, sb, clones[1], "c worked offline")

	before := measureAll(t, cfg, peers, "proj")
	after := setup.ApplySync(cfg, peers, before)

	if log := sb.Git(sb.BaseDir+"/proj", "log", "--oneline", "origin/main"); !strings.Contains(log, "b worked offline") {
		t.Errorf("the first ahead peer was not published:\n%s", log)
	}
	if log := sb.Git(sb.BaseDir+"/proj", "log", "--oneline", "origin/main"); strings.Contains(log, "c worked offline") {
		t.Errorf("a second ahead peer must be left alone, not merged in:\n%s", log)
	}
	if log := sb.Git(clones[0], "log", "--oneline"); strings.Contains(log, "Merge") {
		t.Errorf("initial sync must never create a merge commit on b.local:\n%s", log)
	}
	// c.local's own commit is untouched, still sitting there for the user.
	if log := sb.Git(clones[1], "log", "--oneline"); !strings.Contains(log, "c worked offline") {
		t.Errorf("c.local lost its own commit:\n%s", log)
	} else if strings.Contains(log, "Merge") {
		t.Errorf("initial sync must never create a merge commit on c.local:\n%s", log)
	}

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, after); n == 0 {
		t.Fatalf("RenderSyncResult reported everything level:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "c.local") {
		t.Errorf("result does not mention the peer left alone:\n%s", out.String())
	}
}

// After the repair, a repo that is merely still behind must not be described
// as diverged - that sends the user hunting a merge conflict that is not
// there.
func TestRenderSyncResultDoesNotCallEveryLeftoverDiverged(t *testing.T) {
	repos := []setup.RepoSync{{
		Rel:  "proj",
		Here: setup.SyncPos{Branch: "main", Remote: "github"},
		There: []setup.PeerPos{
			{Peer: config.Peer{Host: "peerhost"}, Pos: setup.SyncPos{Branch: "main", Remote: "github", Behind: 14}},
		},
	}}
	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, repos); n != 1 {
		t.Fatalf("got %d unresolved, want 1", n)
	}
	if strings.Contains(out.String(), "diverged") {
		t.Errorf("a repo that is only behind was reported as diverged:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "still 14 behind") {
		t.Errorf("result does not say what is actually wrong:\n%s", out.String())
	}
}

func TestApplySyncPushesTheFirstMachineThatIsAhead(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.Commit(t, sb, repo, "only here")
	// Two peers, both level with the remote and both behind us. Replies with
	// a real "pos" line rather than a bare success exit: MeasureSync now
	// treats a peer reply with no line for a repo as blocked, not level, so
	// the stub has to answer for real or this test would no longer exercise
	// the "purely ahead, push" path it is named for.
	sb.StubSSHScripted(map[string]string{
		"git-sync-initial-sync": "pos group/proj main origin 0 0\n",
	}, 0)
	cfg := config.Config{BaseDir: sb.BaseDir, Repos: []string{"group/proj"}, Peers: []config.Peer{
		{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"},
	}}
	peers := []setup.PeerTarget{
		{Peer: cfg.Peers[0], BaseDir: "/home/t/code"},
		{Peer: cfg.Peers[1], BaseDir: "/home/t/code"},
	}

	measured, err := setup.MeasureSync(cfg, peers, cfg.Repos)
	if err != nil {
		t.Fatalf("MeasureSync: %v", err)
	}
	if len(measured) != 1 || len(measured[0].There) != 2 {
		t.Fatalf("expected one repo measured on two peers, got %+v", measured)
	}
	setup.ApplySync(cfg, peers, measured)

	// Our commit reached the shared remote, which is the whole point.
	out := sb.Git(repo, "log", "--oneline", "origin/main", "-1")
	if !strings.Contains(out, "only here") {
		t.Errorf("the ahead machine did not push: %s", out)
	}
}

func TestRenderSyncPlanNamesEachMachine(t *testing.T) {
	repos := []setup.RepoSync{{
		Rel:  "group/proj",
		Here: setup.SyncPos{Branch: "main", Remote: "origin", Ahead: 1},
		There: []setup.PeerPos{
			{Peer: config.Peer{Host: "b.local"}, Pos: setup.SyncPos{Branch: "main", Remote: "origin", Behind: 1}},
			{Peer: config.Peer{Host: "c.local"}, Pos: setup.SyncPos{Branch: "main", Remote: "origin", Ahead: 2}},
		},
	}}
	var out bytes.Buffer
	if !setup.RenderSyncPlan(&out, repos) {
		t.Fatal("RenderSyncPlan said there was nothing to do")
	}
	for _, want := range []string{"b.local", "c.local", "group/proj"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan does not mention %q:\n%s", want, out.String())
		}
	}
}
