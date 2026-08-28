// End-to-end tests: the real path across two machines, glued together with a
// loopback "ssh" that runs the peer's side locally against a second, hand
// built ~/.gitsync rather than mocking any of it.
package syncer_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

// buildBinary compiles git-sync once for the e2e tests.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git-sync")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/grillermo/git-sync/cmd/git-sync")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building git-sync: %v\n%s", err, out)
	}
	return bin
}

// peerMachine is a second, unwired machine: its own $HOME, ~/.gitsync and
// base_dir, set up by hand rather than through testutil.NewSandbox, which
// would collide with the machine already occupying this test process's
// environment. Only the loopback ssh stub ever runs anything against it.
type peerMachine struct {
	Home    string
	Gitsync string
	BaseDir string
}

func newPeerMachine(t *testing.T, bin string) *peerMachine {
	t.Helper()
	home := t.TempDir()
	p := &peerMachine{
		Home:    home,
		Gitsync: filepath.Join(home, ".gitsync"),
		BaseDir: filepath.Join(home, "code"),
	}
	testutil.MkdirAll(t, filepath.Join(p.Gitsync, "bin"), filepath.Join(p.Gitsync, "locks"), p.BaseDir)
	copyExecutable(t, bin, filepath.Join(p.Gitsync, "bin", "git-sync"))
	return p
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// saveConfig writes the peer's config.toml directly. config.Config.Save
// would target this test process's own GITSYNC_HOME - machine A's, not the
// peer's - so this builds the same bytes by hand instead.
func (p *peerMachine) saveConfig(t *testing.T, repos []string) {
	t.Helper()
	cfg := config.Config{BaseDir: p.BaseDir, PeerHost: "machineA", PeerUser: "tester", Repos: repos}
	b, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("marshal peer config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p.Gitsync, "config.toml"), b, 0o644); err != nil {
		t.Fatalf("write peer config: %v", err)
	}
}

// clone makes a working clone of rel's bare origin (from sb, "this machine")
// under the peer's base_dir, standing in for the repo already being present
// on the other machine.
func (p *peerMachine) clone(t *testing.T, sb *testutil.Sandbox, rel string) string {
	t.Helper()
	origin := filepath.Join(sb.Home, "remotes", rel+".git")
	dst := filepath.Join(p.BaseDir, rel)
	testutil.MkdirAll(t, filepath.Dir(dst))
	sb.Git(sb.Home, "clone", "-q", origin, dst)
	return dst
}

// cloneNamed is clone, followed by renaming "origin" to remote - the remote
// is the transport, so both machines must agree on its name too.
func (p *peerMachine) cloneNamed(t *testing.T, sb *testutil.Sandbox, rel, remote string) string {
	t.Helper()
	dst := p.clone(t, sb, rel)
	if remote != "origin" {
		sb.Git(dst, "remote", "rename", "origin", remote)
	}
	return dst
}

// events reads the peer's activity log directly, bypassing activity.Read
// (which would read machine A's log, since it goes through this process's
// own GITSYNC_HOME).
func (p *peerMachine) events(t *testing.T) []activity.Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.Gitsync, "activity.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read peer activity log: %v", err)
	}
	var events []activity.Event
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e activity.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad peer event line %q: %v", line, err)
		}
		events = append(events, e)
	}
	return events
}

// installLoopbackSSH puts a fake ssh on PATH that strips ssh's -o flags and
// the user@host target, then runs whatever remote command is left through a
// real shell, with HOME and GITSYNC_HOME pointed at the peer machine. Every
// remote command git-sync ever issues - the receive invocation, and the
// mkdir/cat/mv/git-config sequence a real install sends when provisioning a
// peer - is just a shell command, so one generic stub is enough to answer
// all of them: no per-command special-casing.
func installLoopbackSSH(t *testing.T, sb *testutil.Sandbox, peer *peerMachine) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
while [ "$1" = "-o" ]; do
  shift 2
done
shift
cmd="$1"
HOME=%s GITSYNC_HOME=%s GIT_CONFIG_GLOBAL=%s GIT_CONFIG_SYSTEM=/dev/null \
  GIT_AUTHOR_NAME='git-sync test' GIT_AUTHOR_EMAIL='test@example.com' \
  GIT_COMMITTER_NAME='git-sync test' GIT_COMMITTER_EMAIL='test@example.com' \
  GITSYNC_SECRET_BACKEND=file \
  sh -c "$cmd"
`,
		shellQuote(peer.Home), shellQuote(peer.Gitsync), shellQuote(filepath.Join(peer.Home, ".gitconfig")))

	bin := filepath.Join(sb.Home, "bin")
	testutil.MkdirAll(t, bin)
	path := filepath.Join(bin, "ssh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write ssh stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func assertHasEvent(t *testing.T, events []activity.Event, op activity.Op, status activity.Status, msgSubstr string) {
	t.Helper()
	for _, e := range events {
		if e.Op == op && e.Status == status && strings.Contains(e.Msg, msgSubstr) {
			return
		}
	}
	t.Errorf("no event found with op=%s status=%s msg containing %q; got %+v", op, status, msgSubstr, events)
}

func assertNoErrorEvents(t *testing.T, where string, events []activity.Event) {
	t.Helper()
	for _, e := range events {
		if e.Status == activity.StatusError {
			t.Errorf("unexpected error event on %s: %+v", where, e)
		}
	}
}

// commitTo appends msg to name (not README.md) in repoDir, stages and
// commits. Used instead of testutil.Commit when a test also dirties the
// peer's README.md by hand: two independent edits to the same file at the
// same base revision would conflict on `stash pop`, and that is not what
// this particular test is checking.
func commitTo(t *testing.T, sb *testutil.Sandbox, repoDir, name, msg string) {
	t.Helper()
	testutil.AppendFileIn(t, repoDir, name, msg+"\n")
	sb.Git(repoDir, "add", "-A")
	sb.Git(repoDir, "commit", "-qm", msg)
}

func TestEndToEndCommitReachesThePeer(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newPeerMachine(t, bin)
	peerRepo := peer.clone(t, sb, "group/proj")
	peer.saveConfig(t, []string{"group/proj"})
	installLoopbackSSH(t, sb, peer)

	testutil.Commit(t, sb, repo, "sync me")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	if out := sb.Git(peerRepo, "log", "--oneline", "-1"); !strings.Contains(out, "sync me") {
		t.Errorf("commit did not reach the peer's working tree:\n%s", out)
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "")
	peerEvents := peer.events(t)
	assertHasEvent(t, peerEvents, activity.OpReceive, activity.StatusOK, "fast-forwarded")
	assertNoErrorEvents(t, "peer", peerEvents)
}

func TestEndToEndRepoThePeerDoesNotHave(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newPeerMachine(t, bin)
	// No clone on the peer.
	peer.saveConfig(t, []string{"group/proj"})
	installLoopbackSSH(t, sb, peer)

	testutil.Commit(t, sb, repo, "sync me")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusSkip, "no copy")
	if _, err := os.Stat(filepath.Join(peer.BaseDir, "group/proj")); !os.IsNotExist(err) {
		t.Error("nothing should have been created on the peer")
	}
	peerEvents := peer.events(t)
	assertHasEvent(t, peerEvents, activity.OpReceive, activity.StatusSkip, "not on this machine")
	assertNoErrorEvents(t, "this machine", mustReadActivity(t))
	assertNoErrorEvents(t, "peer", peerEvents)
}

func TestEndToEndPeerHasUncommittedWork(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newPeerMachine(t, bin)
	peerRepo := peer.clone(t, sb, "group/proj")
	peer.saveConfig(t, []string{"group/proj"})
	installLoopbackSSH(t, sb, peer)

	// Dirty the peer's tree: a tracked edit to README.md and an untracked
	// file. The incoming commit touches a different file (see commitTo), so
	// the tracked edit cannot collide with it on stash pop.
	sb.Dirty(peerRepo)

	commitTo(t, sb, repo, "UPDATE.md", "from machine A")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	if out := sb.Git(peerRepo, "log", "--oneline", "-1"); !strings.Contains(out, "from machine A") {
		t.Errorf("did not fast-forward on the peer:\n%s", out)
	}
	testutil.AssertFileContains(t, filepath.Join(peerRepo, "NOTES.md"), "work in progress")
	testutil.AssertFileContains(t, filepath.Join(peerRepo, "README.md"), "uncommitted edit")
	if out := sb.Git(peerRepo, "stash", "list"); out != "" {
		t.Errorf("nothing should be left in the peer's stash, got:\n%s", out)
	}
	assertHasEvent(t, peer.events(t), activity.OpReceive, activity.StatusOK, "restored stashed changes")
}

func TestEndToEndSyncsThroughAGithubNamedRemote(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepoNamedRemote("group/proj", "github")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newPeerMachine(t, bin)
	peerRepo := peer.cloneNamed(t, sb, "group/proj", "github")
	peer.saveConfig(t, []string{"group/proj"})
	installLoopbackSSH(t, sb, peer)

	testutil.Commit(t, sb, repo, "via github remote")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	if out := sb.Git(peerRepo, "log", "--oneline", "-1"); !strings.Contains(out, "via github remote") {
		t.Errorf("commit did not reach the peer's working tree:\n%s", out)
	}
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "github")
	assertHasEvent(t, peer.events(t), activity.OpReceive, activity.StatusOK, "github")
}

// TestEndToEndFeatureBranchCheckedOutBothSidesConvergesDefaultBranch exercises
// the default-branch-anchoring rework end to end: both machines have a
// feature branch checked out, a commit lands on main while it is not the
// checked-out branch on either side, and the real loopback-ssh round trip
// (push -> notify -> receive) still converges main without ever touching
// either machine's checked-out feature branch or working tree.
func TestEndToEndFeatureBranchCheckedOutBothSidesConvergesDefaultBranch(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newPeerMachine(t, bin)
	peerRepo := peer.clone(t, sb, "group/proj")
	peer.saveConfig(t, []string{"group/proj"})
	installLoopbackSSH(t, sb, peer)

	// Both machines check out a feature branch, leaving main behind.
	sb.Git(repo, "checkout", "-q", "-b", "feature")
	sb.Git(peerRepo, "checkout", "-q", "-b", "feature")

	// The commit lands on main, not the checked-out feature branch: hop onto
	// main, commit, then hop back so "feature" is what's checked out when
	// Push runs, exactly like Task 3's default-branch-anchoring covers.
	sb.Git(repo, "checkout", "-q", "main")
	testutil.Commit(t, sb, repo, "sync me via main")
	sb.Git(repo, "checkout", "-q", "feature")

	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	// The peer's local main ref must have advanced even though main was never
	// checked out there.
	if out := sb.Git(peerRepo, "log", "--oneline", "-1", "main"); !strings.Contains(out, "sync me via main") {
		t.Errorf("peer's local main did not fast-forward:\n%s", out)
	}
	// Neither machine's checkout moved off the feature branch.
	if out := sb.Git(repo, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(out) != "feature" {
		t.Errorf("pusher's checked-out branch changed: %q", out)
	}
	if out := sb.Git(peerRepo, "symbolic-ref", "--short", "HEAD"); strings.TrimSpace(out) != "feature" {
		t.Errorf("peer's checked-out branch changed: %q", out)
	}
	// FastForwardRef never touches the worktree: the peer's checked-out
	// feature branch's README must be untouched by main's new commit.
	readme, err := os.ReadFile(filepath.Join(peerRepo, "README.md"))
	if err != nil {
		t.Fatalf("read peer README.md: %v", err)
	}
	if strings.Contains(string(readme), "sync me via main") {
		t.Errorf("peer's working tree was touched by a ref-only fast-forward:\n%s", readme)
	}

	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "main")
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "")
	peerEvents := peer.events(t)
	assertHasEvent(t, peerEvents, activity.OpReceive, activity.StatusOK, "fast-forwarded main")
	assertNoErrorEvents(t, "peer", peerEvents)
}

func TestEndToEndReposOnDifferentRemotesDoNotConverge(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newPeerMachine(t, bin)
	// The peer's clone comes from a *different* bare repo at the same
	// relpath, not sb's own remotes/group/proj.git.
	otherBare := filepath.Join(peer.Home, "other-origin.git")
	sb.Git(sb.Home, "clone", "-q", "--bare", repo, otherBare)
	peerRepo := filepath.Join(peer.BaseDir, "group/proj")
	testutil.MkdirAll(t, filepath.Dir(peerRepo))
	sb.Git(sb.Home, "clone", "-q", otherBare, peerRepo)
	peer.saveConfig(t, []string{"group/proj"})
	installLoopbackSSH(t, sb, peer)

	before := sb.Git(peerRepo, "rev-parse", "HEAD")

	testutil.Commit(t, sb, repo, "only on A's remote")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}
	// The push to A's own remote must still have succeeded.
	testutil.AssertEvent(t, activity.OpPush, activity.StatusOK, "")
	// The peer's remote never saw the commit, so its receive is a no-op, not
	// an error and not a skip for "no copy of this repo" - it has a copy,
	// just of the wrong repository.
	testutil.AssertEvent(t, activity.OpNotify, activity.StatusOK, "")

	after := sb.Git(peerRepo, "rev-parse", "HEAD")
	if before != after {
		t.Error("the peer's tree must not change: its remote never saw the commit")
	}
	if out := sb.Git(peerRepo, "log", "--oneline", "-1"); strings.Contains(out, "only on A's remote") {
		t.Error("the commit must not have reached the peer")
	}

	// This is exactly the case the install-time check exists to catch before
	// it happens: CheckPeerRepos on the same pair must flag it.
	remoteURL := sb.Git(repo, "remote", "get-url", "origin")
	checks, err := setup.CheckPeerReposWithRemotes(
		setup.Target(config.Config{PeerHost: "peerhost", PeerUser: "peeruser"}),
		peer.BaseDir,
		[]setup.RepoWant{{Rel: "group/proj", RemoteURL: strings.TrimSpace(remoteURL)}},
		config.DefaultRemoteNames,
	)
	if err != nil {
		t.Fatalf("CheckPeerReposWithRemotes: %v", err)
	}
	if len(checks) != 1 || checks[0].State != setup.RepoOtherRemote {
		t.Errorf("checks = %+v, want a single RepoOtherRemote", checks)
	}
}

func TestEndToEndInstalledHookFiresOnRealCommit(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")

	peer := newPeerMachine(t, bin)
	installLoopbackSSH(t, sb, peer)

	install := exec.Command(bin, "install", "--peer-host", "peerhost", "--peer-user", "peeruser", "--all", sb.BaseDir)
	install.Env = os.Environ()
	out, err := install.CombinedOutput()
	if err != nil {
		t.Fatalf("git-sync install failed: %v\n%s", err, out)
	}

	commit := exec.Command("git", "commit", "--allow-empty", "-qm", "trigger the real hook")
	commit.Dir = repo
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit in %s: %v\n%s", repo, err, out)
	}

	waitForEvent(t, activity.OpPush, activity.StatusOK, "", 10*time.Second)
	if out := sb.Git(repo, "log", "--oneline", "origin/main"); !strings.Contains(out, "trigger the real hook") {
		t.Errorf("commit did not reach origin without any manual step:\n%s", out)
	}
}

// mustReadActivity reads this process's own activity log (machine A's).
func mustReadActivity(t *testing.T) []activity.Event {
	t.Helper()
	events, err := activity.Read()
	if err != nil {
		t.Fatalf("activity.Read: %v", err)
	}
	return events
}

// waitForEvent polls this process's own activity log for a matching event.
// The hook is deliberately asynchronous, so the event may not exist yet the
// instant the commit returns.
func waitForEvent(t *testing.T, op activity.Op, status activity.Status, msgSubstr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, e := range mustReadActivity(t) {
			if e.Op == op && e.Status == status && strings.Contains(e.Msg, msgSubstr) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for op=%s status=%s msg containing %q", op, status, msgSubstr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
