// End-to-end tests: the real path across a mesh of machines, glued together
// with a loopback "ssh" that routes by hostname to whichever machine's
// locally hand-built ~/.gitsync the command should run against, rather than
// mocking any of it.
package syncer_test

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/lock"
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

// machine is one other machine in the mesh: its own $HOME, ~/.gitsync and
// base_dir, built by hand rather than through testutil.NewSandbox, which
// would collide with the machine already occupying this test process's
// environment. Only the loopback ssh stub ever runs anything against it.
type machine struct {
	Name    string // the hostname the stub routes on
	Home    string
	Gitsync string
	BaseDir string
}

func newMachine(t *testing.T, bin, name string) *machine {
	t.Helper()
	home := t.TempDir()
	m := &machine{
		Name:    name,
		Home:    home,
		Gitsync: filepath.Join(home, ".gitsync"),
		BaseDir: filepath.Join(home, "code"),
	}
	testutil.MkdirAll(t, filepath.Join(m.Gitsync, "bin"), filepath.Join(m.Gitsync, "locks"), m.BaseDir)
	copyExecutable(t, bin, filepath.Join(m.Gitsync, "bin", "git-sync"))
	return m
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

// saveConfig writes this machine's config.toml directly: config.Save would
// target the test process's own GITSYNC_HOME, which belongs to machine A.
func (m *machine) saveConfig(t *testing.T, repos []string, peers []config.Peer) {
	t.Helper()
	cfg := config.Config{BaseDir: m.BaseDir, Repos: repos, Peers: peers}
	b, err := cfg.MarshalFor(m.Name)
	if err != nil {
		t.Fatalf("marshal %s config: %v", m.Name, err)
	}
	if err := os.WriteFile(filepath.Join(m.Gitsync, "config.toml"), b, 0o644); err != nil {
		t.Fatalf("write %s config: %v", m.Name, err)
	}
}

// clone makes a working clone of rel's bare origin (from sb, "this machine")
// under this machine's base_dir, standing in for the repo already being
// present on the other machine.
func (m *machine) clone(t *testing.T, sb *testutil.Sandbox, rel string) string {
	t.Helper()
	origin := filepath.Join(sb.Home, "remotes", rel+".git")
	dst := filepath.Join(m.BaseDir, rel)
	testutil.MkdirAll(t, filepath.Dir(dst))
	sb.Git(sb.Home, "clone", "-q", origin, dst)
	return dst
}

// cloneNamed is clone, followed by renaming "origin" to remote - the remote
// is the transport, so every machine must agree on its name too.
func (m *machine) cloneNamed(t *testing.T, sb *testutil.Sandbox, rel, remote string) string {
	t.Helper()
	dst := m.clone(t, sb, rel)
	if remote != "origin" {
		sb.Git(dst, "remote", "rename", "origin", remote)
	}
	return dst
}

// events reads this machine's activity log directly, bypassing activity.Read
// (which would read machine A's log, since it goes through this process's
// own GITSYNC_HOME).
func (m *machine) events(t *testing.T) []activity.Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.Gitsync, "activity.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s activity log: %v", m.Name, err)
	}
	var events []activity.Event
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e activity.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad %s event line %q: %v", m.Name, line, err)
		}
		events = append(events, e)
	}
	return events
}

// installLoopbackSSH puts a fake ssh on PATH that strips ssh's -o flags,
// routes on the user@host target, and runs the remaining command through a
// real shell with HOME and GITSYNC_HOME pointed at that machine. Every remote
// command git-sync issues - receive, the provisioning mkdir/cat/mv sequence,
// and a peer's own ssh to another peer during the key check - is just a shell
// command string, so one routing stub answers all of them.
func installLoopbackSSH(t *testing.T, sb *testutil.Sandbox, machines ...*machine) {
	t.Helper()
	var cases strings.Builder
	for _, m := range machines {
		fmt.Fprintf(&cases, `  *%s*) HOME=%s; GITSYNC_HOME=%s; GITCONFIG=%s ;;
`, m.Name, shellQuote(m.Home), shellQuote(m.Gitsync),
			shellQuote(filepath.Join(m.Home, ".gitconfig")))
	}
	script := fmt.Sprintf(`#!/bin/sh
while [ "$1" = "-o" ]; do
  shift 2
done
target="$1"
shift
case "$target" in
%s  *) echo "loopback ssh: unknown host $target" >&2; exit 255 ;;
esac
cmd="$1"
HOME="$HOME" GITSYNC_HOME="$GITSYNC_HOME" \
  GIT_CONFIG_GLOBAL="$GITCONFIG" GIT_CONFIG_SYSTEM=/dev/null \
  GIT_AUTHOR_NAME='git-sync test' GIT_AUTHOR_EMAIL='test@example.com' \
  GIT_COMMITTER_NAME='git-sync test' GIT_COMMITTER_EMAIL='test@example.com' \
  PATH="$PATH" \
  sh -c "$cmd"
`, cases.String())

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

func TestEndToEndCommitReachesEveryMachine(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")

	b := newMachine(t, bin, "b.local")
	c := newMachine(t, bin, "c.local")
	bRepo := b.clone(t, sb, "group/proj")
	cRepo := c.clone(t, sb, "group/proj")
	peers := []config.Peer{{Host: "b.local", User: "tester"}, {Host: "c.local", User: "tester"}}
	testutil.SaveConfigWithPeers(t, sb, peers, []string{"group/proj"})
	b.saveConfig(t, []string{"group/proj"}, append([]config.Peer{{Host: "a.local", User: "tester"}}, peers[1]))
	c.saveConfig(t, []string{"group/proj"}, append([]config.Peer{{Host: "a.local", User: "tester"}}, peers[0]))
	installLoopbackSSH(t, sb, b, c)

	testutil.Commit(t, sb, repo, "sync me")
	if code := syncer.Push("group/proj"); code != 0 {
		t.Fatalf("Push = %d, want 0", code)
	}

	for name, dir := range map[string]string{"b.local": bRepo, "c.local": cRepo} {
		if out := sb.Git(dir, "log", "--oneline", "-1"); !strings.Contains(out, "sync me") {
			t.Errorf("commit did not reach %s:\n%s", name, out)
		}
	}
	assertHasEvent(t, b.events(t), activity.OpReceive, activity.StatusOK, "fast-forwarded")
	assertHasEvent(t, c.events(t), activity.OpReceive, activity.StatusOK, "fast-forwarded")
}

func TestEndToEndCommitIsRefusedWhileReceiving(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	other := sb.MakeRepo("group/other")
	testutil.SaveConfigWithPeers(t, sb,
		[]config.Peer{{Host: "b.local", User: "tester"}},
		[]string{"group/proj", "group/other"})

	// Install for real, so git runs the hooks we ship.
	if err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, Repos: []string{"group/proj", "group/other"},
		Self: bin, NoPeer: true, Out: io.Discard,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	l, err := lock.AcquireFrom("group/proj", "b.local", time.Second)
	if err != nil {
		t.Fatalf("AcquireFrom: %v", err)
	}
	defer l.Release()

	testutil.AppendFileIn(t, repo, "README.md", "edit\n")
	cmd := exec.Command("git", "commit", "-am", "should be refused")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("commit succeeded during a receive:\n%s", out)
	}
	if !strings.Contains(string(out), "b.local") {
		t.Errorf("git did not show git-sync's message:\n%s", out)
	}

	// A different repo is unaffected: the lock is per repo.
	testutil.AppendFileIn(t, other, "README.md", "edit\n")
	cmd = exec.Command("git", "commit", "-am", "allowed")
	cmd.Dir = other
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("an unrelated repo was blocked:\n%s", out)
	}
	// The post-commit hook just fired for real and spawned a detached
	// background push (group/other is installed and selected too). Wait for
	// it to finish before the sandbox's temp dirs are torn down, or the
	// still-running child can race t.TempDir's cleanup and fail it with
	// "directory not empty".
	waitForEvent(t, activity.OpPush, activity.StatusOK, "", 10*time.Second)
}

func TestEndToEndCommitReachesThePeer(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newMachine(t, bin, "peerhost")
	peerRepo := peer.clone(t, sb, "group/proj")
	peer.saveConfig(t, []string{"group/proj"}, []config.Peer{{Host: "machineA", User: "tester"}})
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

	peer := newMachine(t, bin, "peerhost")
	// No clone on the peer.
	peer.saveConfig(t, []string{"group/proj"}, []config.Peer{{Host: "machineA", User: "tester"}})
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

	peer := newMachine(t, bin, "peerhost")
	peerRepo := peer.clone(t, sb, "group/proj")
	peer.saveConfig(t, []string{"group/proj"}, []config.Peer{{Host: "machineA", User: "tester"}})
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

	peer := newMachine(t, bin, "peerhost")
	peerRepo := peer.cloneNamed(t, sb, "group/proj", "github")
	peer.saveConfig(t, []string{"group/proj"}, []config.Peer{{Host: "machineA", User: "tester"}})
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

func TestEndToEndReposOnDifferentRemotesDoNotConverge(t *testing.T) {
	bin := buildBinary(t)
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peerhost", "peeruser")

	peer := newMachine(t, bin, "peerhost")
	// The peer's clone comes from a *different* bare repo at the same
	// relpath, not sb's own remotes/group/proj.git.
	otherBare := filepath.Join(peer.Home, "other-origin.git")
	sb.Git(sb.Home, "clone", "-q", "--bare", repo, otherBare)
	peerRepo := filepath.Join(peer.BaseDir, "group/proj")
	testutil.MkdirAll(t, filepath.Dir(peerRepo))
	sb.Git(sb.Home, "clone", "-q", otherBare, peerRepo)
	peer.saveConfig(t, []string{"group/proj"}, []config.Peer{{Host: "machineA", User: "tester"}})
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

	peer := newMachine(t, bin, "peerhost")
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
