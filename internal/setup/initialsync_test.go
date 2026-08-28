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
// temp tree for this process, and the stub's children inherit that, so the
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

// syncFixture sets up one repo present on both machines, with the peer clone
// standing in for the other machine's copy. Returns the config to measure
// with, the peer's base dir, and the peer clone's path.
func syncFixture(t *testing.T, sb *testutil.Sandbox, rel string) (config.Config, string, string) {
	t.Helper()
	sb.MakeRepo(rel)
	peer := sb.PeerClone(rel)
	installLoopbackSSH(t, sb)
	cfg := config.Config{BaseDir: sb.BaseDir, PeerHost: "peerhost", PeerUser: "peeruser"}
	return cfg, filepath.Join(sb.Home, "peer"), peer
}

// commitNoPush makes a commit in dir and deliberately leaves it unpushed -
// the state that caused a real sync to silently stop converging.
func commitNoPush(t *testing.T, sb *testutil.Sandbox, dir, msg string) {
	t.Helper()
	testutil.AppendFileIn(t, dir, "LOCAL.md", msg+"\n")
	sb.Git(dir, "add", "-A")
	sb.Git(dir, "commit", "-qm", msg)
}

func measure(t *testing.T, cfg config.Config, peerBase string, repos ...string) []setup.RepoSync {
	t.Helper()
	got, err := setup.MeasureSync(setup.Target(cfg), peerBase, cfg, repos)
	if err != nil {
		t.Fatalf("MeasureSync: %v", err)
	}
	return got
}

func TestMeasureSyncSeesBothMachinesLevel(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, _ := syncFixture(t, sb, "proj")

	got := measure(t, cfg, peerBase, "proj")
	if len(got) != 1 {
		t.Fatalf("got %d repos, want 1", len(got))
	}
	if got[0].Here.Ahead != 0 || got[0].Here.Behind != 0 {
		t.Errorf("here = %+v, want level", got[0].Here)
	}
	if got[0].There.Ahead != 0 || got[0].There.Behind != 0 {
		t.Errorf("there = %+v, want level", got[0].There)
	}

	var out bytes.Buffer
	if setup.RenderSyncPlan(&out, "peerhost", got) {
		t.Errorf("RenderSyncPlan reported work to do:\n%s", out.String())
	}
}

// The regression test for the incident this stage exists to prevent: the peer
// held a commit it had never pushed, so every later receive refused the
// fast-forward and warned, forever, with nothing retrying it.
func TestInitialSyncPushesTheUnpushedPeerCommit(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")
	commitNoPush(t, sb, peer, "peer worked offline")

	before := measure(t, cfg, peerBase, "proj")
	if before[0].There.Ahead != 1 {
		t.Fatalf("peer ahead = %d, want 1", before[0].There.Ahead)
	}
	if before[0].Here.Behind != 0 {
		t.Fatalf("here behind = %d, want 0 (the commit is not on the remote yet)", before[0].Here.Behind)
	}

	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg, before)

	if after[0].There.Ahead != 0 || after[0].There.Behind != 0 {
		t.Errorf("peer = %+v, want level after the initial sync", after[0].There)
	}
	if after[0].Here.Ahead != 0 || after[0].Here.Behind != 0 {
		t.Errorf("here = %+v, want level after the initial sync", after[0].Here)
	}
	if log := sb.Git(sb.BaseDir+"/proj", "log", "--oneline"); !strings.Contains(log, "peer worked offline") {
		t.Errorf("this machine never got the peer's commit:\n%s", log)
	}

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", after); n != 0 {
		t.Errorf("RenderSyncResult = %d unresolved, want 0:\n%s", n, out.String())
	}
}

func TestInitialSyncPushesOurCommitAndThePeerLandsIt(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")
	commitNoPush(t, sb, sb.BaseDir+"/proj", "committed before install")

	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg,
		measure(t, cfg, peerBase, "proj"))

	if after[0].Here.Ahead != 0 || after[0].There.Ahead != 0 || after[0].There.Behind != 0 {
		t.Errorf("after = %+v / %+v, want both level", after[0].Here, after[0].There)
	}
	if log := sb.Git(peer, "log", "--oneline"); !strings.Contains(log, "committed before install") {
		t.Errorf("peer never got our commit:\n%s", log)
	}
}

// conflictingCommit overwrites README.md's one line with different content
// than any other conflictingCommit call and commits it, without pushing.
// Two of these on independent histories always conflict on merge, unlike two
// independent appends (which git merges cleanly on its own) - needed now
// that a genuinely diverged default branch gets one real merge attempt at
// install time, and a test asserting "left diverged for the user" has to
// actually be unmergeable, not just unpushed.
func conflictingCommit(t *testing.T, sb *testutil.Sandbox, dir, content string) {
	t.Helper()
	testutil.WriteFileIn(t, dir, "README.md", content+"\n")
	sb.Git(dir, "add", "-A")
	sb.Git(dir, "commit", "-qm", content)
}

// Push and fast-forward cannot reconcile two machines that each have commits
// the other lacks, and here the two sides also edited the same line, so the
// one merge attempt install-time convergence is allowed conflicts. git-sync
// must say so rather than force anything: resolving a conflict is the user's
// call, and a merge commit here would be one nobody asked for.
func TestInitialSyncLeavesDivergedHistoryForTheUser(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")
	conflictingCommit(t, sb, sb.BaseDir+"/proj", "ours")
	conflictingCommit(t, sb, peer, "theirs")

	// Getting one side onto the remote is what makes the other diverged.
	sb.Git(sb.BaseDir+"/proj", "push", "-q", "origin", "main")

	got := measure(t, cfg, peerBase, "proj")
	if got[0].There.Ahead == 0 || got[0].There.Behind == 0 {
		t.Fatalf("peer = %+v, want it diverged (both ahead and behind)", got[0].There)
	}
	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg, got)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", after); n != 1 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 1:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "diverged") {
		t.Errorf("result does not explain the divergence:\n%s", out.String())
	}
	if log := sb.Git(peer, "log", "--oneline"); strings.Contains(log, "Merge") {
		t.Errorf("initial sync created a merge commit on the peer:\n%s", log)
	}
	if log := sb.Git(peer, "log", "--oneline"); !strings.Contains(log, "theirs") {
		t.Errorf("peer lost its own commit:\n%s", log)
	}
	if status := sb.Git(peer, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("peer working tree not clean after the aborted merge: %q", status)
	}
}

// The two machines only meeting on the same branch used to be the only
// reason ApplySync would fully skip a repo. A feature branch checked out on
// both sides must not stop the default branch underneath it from levelling -
// that used to require @{upstream} or CurrentBranch, both of which follow
// whatever is checked out rather than the shared remote's HEAD.
func TestInitialSyncLevelsDefaultBranchWithFeatureCheckedOut(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")
	local := sb.BaseDir + "/proj"

	// Advance main, then leave a feature branch checked out on both sides -
	// on the local machine, one commit ahead of what has been pushed.
	commitNoPush(t, sb, local, "advance main")
	sb.Git(local, "checkout", "-q", "-b", "feature")
	sb.Git(peer, "checkout", "-q", "-b", "feature-peer")

	got := measure(t, cfg, peerBase, "proj")
	if got[0].Here.Branch != "main" || got[0].There.Branch != "main" {
		t.Fatalf("branches = %q / %q, want both main despite the feature branches checked out",
			got[0].Here.Branch, got[0].There.Branch)
	}
	if got[0].Here.Ahead != 1 {
		t.Fatalf("here ahead = %d, want 1 (the unpushed main commit)", got[0].Here.Ahead)
	}

	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg, got)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", after); n != 0 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 0:\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "different branches") {
		t.Errorf("the different-branches blocker fired despite both resolving main:\n%s", out.String())
	}
	hereMain := sb.Git(local, "rev-parse", "main")
	peerMain := sb.Git(peer, "rev-parse", "main")
	if hereMain != peerMain {
		t.Errorf("main = %q here, %q on peer, want equal", hereMain, peerMain)
	}
	// Neither side's feature branch checkout should have moved.
	if head := sb.Git(local, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(head) != "feature" {
		t.Errorf("local HEAD moved off the feature branch: %q", head)
	}
}

// The install-time exception to "never auto-merge": a genuinely diverged
// default branch whose changes do not conflict gets merged, pushed, and the
// other side fast-forwards onto it - all within one ApplySync call.
func TestInitialSyncCleanMergeConverges(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")
	local := sb.BaseDir + "/proj"

	// The peer's commit lands on the shared remote first, then this machine
	// commits something unrelated without pushing: diverged, but on
	// different files, so a merge has nothing to conflict over.
	testutil.AppendFileIn(t, peer, "PEER.md", "peer work\n")
	sb.Git(peer, "add", "-A")
	sb.Git(peer, "commit", "-qm", "peer work")
	sb.Git(peer, "push", "-q", "origin", "main")
	commitNoPush(t, sb, local, "local work")

	before := measure(t, cfg, peerBase, "proj")
	if before[0].Here.Ahead == 0 || before[0].Here.Behind == 0 {
		t.Fatalf("here = %+v, want it diverged (both ahead and behind)", before[0].Here)
	}

	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg, before)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", after); n != 0 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 0:\n%s", n, out.String())
	}
	hereMain := sb.Git(local, "rev-parse", "main")
	peerMain := sb.Git(peer, "rev-parse", "main")
	if hereMain != peerMain {
		t.Errorf("main = %q here, %q on peer, want equal after the clean merge converges", hereMain, peerMain)
	}
	if log := sb.Git(local, "log", "--oneline"); !strings.Contains(log, "Merge") {
		t.Errorf("no merge commit was created here:\n%s", log)
	}
	if log := sb.Git(local, "log", "--oneline"); !strings.Contains(log, "local work") ||
		!strings.Contains(log, "peer work") {
		t.Errorf("merged history is missing one side's commit:\n%s", log)
	}
}

// A conflicting merge must never be left half-done: the abort has to restore
// a clean working tree and leave no new commit, and the repo still has to be
// reported as needing the user's attention.
func TestInitialSyncConflictAbortsAndReports(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")
	local := sb.BaseDir + "/proj"

	conflictingCommit(t, sb, peer, "theirs")
	sb.Git(peer, "push", "-q", "origin", "main")
	beforeHead := sb.Git(local, "rev-parse", "HEAD")
	conflictingCommit(t, sb, local, "ours")

	before := measure(t, cfg, peerBase, "proj")
	if before[0].Here.Ahead == 0 || before[0].Here.Behind == 0 {
		t.Fatalf("here = %+v, want it diverged (both ahead and behind)", before[0].Here)
	}

	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg, before)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", after); n != 1 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 1:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "diverged") {
		t.Errorf("result does not explain the divergence:\n%s", out.String())
	}
	if status := sb.Git(local, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Errorf("working tree not clean after the aborted merge: %q", status)
	}
	if head := sb.Git(local, "rev-parse", "HEAD"); head == beforeHead {
		t.Fatalf("test setup did not actually commit locally") // sanity check on the fixture itself
	}
	if log := sb.Git(local, "log", "--oneline"); strings.Contains(log, "Merge") {
		t.Errorf("a merge commit was left behind after the abort:\n%s", log)
	}
	if _, err := os.Stat(local + "/.git/MERGE_HEAD"); err == nil {
		t.Errorf("MERGE_HEAD still present, the merge was not fully aborted")
	}
}

// The mirror image of the clean-merge test: the peer is the diverged side,
// so the merge has to happen inside syncApplyScript's shell, over ssh, not
// in this process. Exercises the shell script's new diverged branch through
// the same loopback ssh stub the other initial-sync tests use.
func TestInitialSyncPeerSideCleanMerge(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")
	local := sb.BaseDir + "/proj"

	// This machine's commit will be purely ahead until it is pushed; the
	// peer's own unrelated, unpushed commit is what makes it diverged once
	// our commit reaches the remote.
	commitNoPush(t, sb, local, "local work")
	testutil.AppendFileIn(t, peer, "PEER.md", "peer work\n")
	sb.Git(peer, "add", "-A")
	sb.Git(peer, "commit", "-qm", "peer work")

	before := measure(t, cfg, peerBase, "proj")
	if before[0].Here.Ahead != 1 || before[0].Here.Behind != 0 {
		t.Fatalf("here = %+v, want purely ahead by 1", before[0].Here)
	}
	if before[0].There.Ahead != 1 || before[0].There.Behind != 0 {
		t.Fatalf("there = %+v, want purely ahead by 1 (not yet diverged - we have not pushed)", before[0].There)
	}

	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg, before)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", after); n != 0 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 0:\n%s", n, out.String())
	}
	hereMain := sb.Git(local, "rev-parse", "main")
	peerMain := sb.Git(peer, "rev-parse", "main")
	if hereMain != peerMain {
		t.Errorf("main = %q here, %q on peer, want equal", hereMain, peerMain)
	}
	if log := sb.Git(peer, "log", "--oneline"); !strings.Contains(log, "Merge") {
		t.Errorf("no merge commit was created on the peer:\n%s", log)
	}
	if log := sb.Git(peer, "log", "--oneline"); !strings.Contains(log, "local work") ||
		!strings.Contains(log, "peer work") {
		t.Errorf("merged history on the peer is missing one side's commit:\n%s", log)
	}
}

// The two machines only ever meet on the same branch, so a mismatch is
// reported rather than quietly half-synced.
// Now that measurement anchors on the remote's default branch rather than
// whatever is checked out, two machines resolving different branches can
// only happen if they are pointed at genuinely different remotes - the
// "cloned from a different remote" case CLAUDE.md's invariants call out,
// not a difference in what is checked out. That is exotic enough that it is
// simpler, and no less faithful, to exercise the reporting functions
// directly with a hand-built mismatch than to construct two distinct bare
// remotes end to end.
func TestInitialSyncReportsDifferentBranches(t *testing.T) {
	got := []setup.RepoSync{{
		Rel:   "proj",
		Here:  setup.SyncPos{Branch: "main", Remote: "origin"},
		There: setup.SyncPos{Branch: "other", Remote: "origin"},
	}}

	var out bytes.Buffer
	if !setup.RenderSyncPlan(&out, "peerhost", got) {
		t.Fatalf("plan reported nothing to do:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "different branches") {
		t.Errorf("plan does not name the branch mismatch:\n%s", out.String())
	}

	out.Reset()
	if n := setup.RenderSyncResult(&out, "peerhost", got); n != 1 {
		t.Errorf("RenderSyncResult = %d unresolved, want 1", n)
	}
}

// A repo the peer has never cloned must not be reported as fine, and must not
// stop the other repos from being levelled.
func TestInitialSyncMarksARepoThePeerDoesNotHave(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, _ := syncFixture(t, sb, "proj")
	sb.MakeRepo("solo")

	got := measure(t, cfg, peerBase, "proj", "solo")
	if len(got) != 2 {
		t.Fatalf("got %d repos, want 2", len(got))
	}
	if got[1].There.Err == "" {
		t.Errorf("solo peer position = %+v, want an error", got[1].There)
	}

	var out bytes.Buffer
	setup.RenderSyncPlan(&out, "peerhost", got)
	if !strings.Contains(out.String(), "solo") {
		t.Errorf("plan does not mention the one-sided repo:\n%s", out.String())
	}
}

// A dirty working tree must not be the reason a repo is left unlevelled: the
// steady-state receive stashes around the fast-forward, so the initial sync
// has to as well. Found by running the real thing against a real peer, which
// had one edited file and stayed 14 commits behind.
func TestInitialSyncLevelsARepoWithADirtyTree(t *testing.T) {
	sb := testutil.NewSandbox(t)
	cfg, peerBase, peer := syncFixture(t, sb, "proj")

	// The peer is behind, and has an unrelated uncommitted edit.
	commitNoPush(t, sb, sb.BaseDir+"/proj", "new work")
	sb.Git(sb.BaseDir+"/proj", "push", "-q", "origin", "main")
	testutil.AppendFileIn(t, peer, "SCRATCH.md", "work in progress\n")

	before := measure(t, cfg, peerBase, "proj")
	if before[0].There.Behind == 0 {
		t.Fatalf("peer = %+v, want it behind", before[0].There)
	}

	after := setup.ApplySync(setup.Target(cfg), peerBase, cfg, before)

	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", after); n != 0 {
		t.Fatalf("RenderSyncResult = %d unresolved, want 0:\n%s", n, out.String())
	}
	if log := sb.Git(peer, "log", "--oneline"); !strings.Contains(log, "new work") {
		t.Errorf("peer did not fast-forward:\n%s", log)
	}
	// The stash must have come back.
	if b, err := os.ReadFile(filepath.Join(peer, "SCRATCH.md")); err != nil ||
		!strings.Contains(string(b), "work in progress") {
		t.Errorf("uncommitted work was not restored on the peer: %v", err)
	}
}

// After the repair, a repo that is merely still behind must not be described
// as diverged - that sends the user hunting a merge conflict that is not there.
func TestRenderSyncResultDoesNotCallEveryLeftoverDiverged(t *testing.T) {
	repos := []setup.RepoSync{{
		Rel:   "proj",
		Here:  setup.SyncPos{Branch: "main", Remote: "github"},
		There: setup.SyncPos{Branch: "main", Remote: "github", Behind: 14},
	}}
	var out bytes.Buffer
	if n := setup.RenderSyncResult(&out, "peerhost", repos); n != 1 {
		t.Fatalf("got %d unresolved, want 1", n)
	}
	if strings.Contains(out.String(), "diverged") {
		t.Errorf("a repo that is only behind was reported as diverged:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "still 14 behind") {
		t.Errorf("result does not say what is actually wrong:\n%s", out.String())
	}
}
