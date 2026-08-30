package setup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

// loopbackSSH puts a fake ssh on PATH that strips ssh's -o flags and the
// user@host target, then runs the remote command through a real shell. The
// clone stage's remote half is a shell script that has to actually work, so
// these tests run it rather than asserting on its text.
func loopbackSSH(t *testing.T, sb *testutil.Sandbox) {
	t.Helper()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$GITSYNC_HOME/ssh-calls.log"
while [ "$1" = "-o" ]; do
  shift 2
done
shift
sh -c "$1"
`
	bin := filepath.Join(sb.Home, "bin")
	testutil.MkdirAll(t, bin)
	path := filepath.Join(bin, "ssh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write ssh stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// originOf is the URL testutil.MakeRepo gives rel's remote.
func originOf(sb *testutil.Sandbox, rel string) string {
	return filepath.Join(sb.Home, "remotes", rel+".git")
}

func missing(rel, url string) setup.RepoCheck {
	return setup.RepoCheck{Rel: rel, State: setup.RepoMissing, RemoteURL: url}
}

func TestCloneMissingClonesTheRepoOnThePeer(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("work/api")
	loopbackSSH(t, sb)
	peerBase := filepath.Join(sb.Home, "peer")

	got := setup.CloneMissing("u@h", peerBase, config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("work/api", originOf(sb, "work/api"))})

	if len(got) != 1 || !got[0].Cloned || got[0].Err != "" {
		t.Fatalf("results = %+v, want work/api cloned", got)
	}
	if _, err := os.Stat(filepath.Join(peerBase, "work/api", ".git")); err != nil {
		t.Fatalf("the peer has no clone at the same relative path: %v", err)
	}
}

func TestCloneMissingPushesBeforeCloning(t *testing.T) {
	// The clone can only contain what the remote already has. A local commit
	// that was never pushed would leave the peer's brand new clone behind from
	// the moment it existed - the exact state initial sync exists to prevent.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("notes")
	testutil.Commit(t, sb, repo, "not pushed yet")
	loopbackSSH(t, sb)
	peerBase := filepath.Join(sb.Home, "peer")

	got := setup.CloneMissing("u@h", peerBase, config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("notes", originOf(sb, "notes"))})
	if !got[0].Cloned {
		t.Fatalf("results = %+v, want notes cloned", got)
	}
	if got[0].PushNote != "" {
		t.Errorf("PushNote = %q, want the push to have gone through", got[0].PushNote)
	}
	out := sb.Git(filepath.Join(peerBase, "notes"), "log", "--oneline")
	if !strings.Contains(out, "not pushed yet") {
		t.Errorf("the peer's clone is missing this machine's commit:\n%s", out)
	}
}

func TestCloneMissingKeepsTheRemoteName(t *testing.T) {
	// Both machines resolve the remote by name, github before origin. A clone
	// that named it origin while this machine calls it github would still work
	// by URL, but the two would be describing the same remote differently in
	// every later message.
	sb := testutil.NewSandbox(t)
	sb.MakeRepoNamedRemote("gh", "github")
	loopbackSSH(t, sb)
	peerBase := filepath.Join(sb.Home, "peer")

	got := setup.CloneMissing("u@h", peerBase, config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("gh", originOf(sb, "gh"))})
	if !got[0].Cloned {
		t.Fatalf("results = %+v, want gh cloned", got)
	}
	if out := sb.Git(filepath.Join(peerBase, "gh"), "remote"); strings.TrimSpace(out) != "github" {
		t.Errorf("the peer's remote is named %q, want github", strings.TrimSpace(out))
	}
}

func TestCloneMissingNeverWritesOverAnExistingPath(t *testing.T) {
	// The state changed between the check and now, or the check was wrong.
	// Either way, a directory we did not classify as missing is not ours.
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("work/api")
	loopbackSSH(t, sb)
	peerBase := filepath.Join(sb.Home, "peer")
	occupied := filepath.Join(peerBase, "work/api")
	testutil.MkdirAll(t, occupied)
	testutil.WriteFileIn(t, occupied, "keep-me", "precious\n")

	got := setup.CloneMissing("u@h", peerBase, config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("work/api", originOf(sb, "work/api"))})

	if got[0].Cloned || got[0].Err == "" {
		t.Fatalf("results = %+v, want work/api reported as not cloned", got)
	}
	if _, err := os.Stat(filepath.Join(occupied, "keep-me")); err != nil {
		t.Fatalf("an existing file was destroyed: %v", err)
	}
}

func TestCloneMissingSendsOneCommandForAllRepos(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("a")
	sb.MakeRepo("b")
	loopbackSSH(t, sb)
	peerBase := filepath.Join(sb.Home, "peer")

	setup.CloneMissing("u@h", peerBase, config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("a", originOf(sb, "a")), missing("b", originOf(sb, "b"))})

	if n := strings.Count(sb.SSHCalls(), "git-sync-clone-missing"); n != 1 {
		t.Errorf("sent %d clone commands, want 1", n)
	}
}

func TestCloneMissingReportsARepoWithNoRemoteHere(t *testing.T) {
	// Nothing to clone from, and no remote name to give the peer: report it
	// rather than inventing one.
	sb := testutil.NewSandbox(t)
	testutil.MkdirAll(t, filepath.Join(sb.BaseDir, "solo"))
	sb.Git(sb.BaseDir, "init", "-q", filepath.Join(sb.BaseDir, "solo"))
	loopbackSSH(t, sb)

	got := setup.CloneMissing("u@h", filepath.Join(sb.Home, "peer"),
		config.Config{BaseDir: sb.BaseDir}, []setup.RepoCheck{missing("solo", "u")})

	if got[0].Cloned || !strings.Contains(got[0].Err, "no remote") {
		t.Errorf("results = %+v, want solo reported as having no remote", got)
	}
	if strings.Contains(sb.SSHCalls(), "git-sync-clone-missing") {
		t.Errorf("nothing clonable means nothing to send:\n%s", sb.SSHCalls())
	}
}

func TestCloneMissingReportsAnUnreachablePeer(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("a")
	sb.StubSSHScripted(nil, 255)

	got := setup.CloneMissing("u@h", "/home/peer/code", config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("a", originOf(sb, "a"))})
	if got[0].Cloned || got[0].Err == "" {
		t.Errorf("results = %+v, want a reported as not cloned", got)
	}
}

func TestCloneMissingReportsARepoTheAnswerOmitted(t *testing.T) {
	// A truncated or garbled answer must not silently become "cloned".
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("a")
	sb.MakeRepo("b")
	sb.StubSSHScripted(map[string]string{"git-sync-clone-missing": "ok a\n"}, 0)

	got := setup.CloneMissing("u@h", "/home/peer/code", config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("a", originOf(sb, "a")), missing("b", originOf(sb, "b"))})
	if !got[0].Cloned {
		t.Errorf("results = %+v, want a cloned", got)
	}
	if got[1].Cloned || got[1].Err == "" {
		t.Errorf("results = %+v, want b reported as unexplained", got)
	}
}

func TestClonableSkipsWhatCloningCannotFix(t *testing.T) {
	// Only an empty path with a known remote URL. A directory that is not a
	// repo, or a clone of a different repository, is the user's to sort out -
	// cloning over either would destroy something we do not understand.
	checks := []setup.RepoCheck{
		missing("fix-me", "git@github.com:me/fix-me.git"),
		{Rel: "notes", State: setup.RepoPresent, RemoteURL: "u"},
		{Rel: "scratch", State: setup.RepoNotAGitRepo, RemoteURL: "u"},
		{Rel: "gh", State: setup.RepoOtherRemote, RemoteURL: "u", PeerRemoteURL: "v"},
		{Rel: "hidden", State: setup.RepoNoRemote, RemoteURL: "u"},
		{Rel: "quiet", State: setup.RepoUnchecked, RemoteURL: "u"},
		missing("no-url", ""),
		missing("it's/bad", "u"),
		missing("weird", "u'rl"),
	}
	got := setup.ClonableChecks(checks)
	if len(got) != 1 || got[0].Rel != "fix-me" {
		t.Errorf("clonable = %+v, want only fix-me", got)
	}
}

func TestCloneMissingNeverSendsAQuotedValue(t *testing.T) {
	// The rel and the URL are interpolated into a single-quoted remote command,
	// exactly as everywhere else in this package.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(nil, 0)

	setup.CloneMissing("u@h", "/home/peer/code", config.Config{BaseDir: sb.BaseDir},
		[]setup.RepoCheck{missing("it's/bad", "u")})
	if strings.Contains(sb.SSHCalls(), "it's/bad") {
		t.Errorf("a quoted relpath must never reach the remote command:\n%s", sb.SSHCalls())
	}
}

func TestRenderClonePlanNamesEveryRepoAndItsRemote(t *testing.T) {
	var out strings.Builder
	if !setup.RenderClonePlan(&out, "peerbox", "/home/peer/code",
		[]setup.RepoCheck{missing("work/api", "git@github.com:me/api.git")}) {
		t.Fatal("RenderClonePlan = false, want true when there is something to clone")
	}
	for _, want := range []string{"work/api", "git@github.com:me/api.git", "peerbox", "/home/peer/code"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, out.String())
		}
	}
}

func TestRenderClonePlanSaysNothingWithNothingToDo(t *testing.T) {
	var out strings.Builder
	if setup.RenderClonePlan(&out, "peerbox", "/home/peer/code", nil) {
		t.Error("RenderClonePlan = true with nothing to clone")
	}
	if out.String() != "" {
		t.Errorf("nothing to clone should print nothing here:\n%s", out.String())
	}
}

func TestRenderCloneResultReportsBothOutcomes(t *testing.T) {
	var out strings.Builder
	n := setup.RenderCloneResult(&out, "peerbox", []setup.CloneResult{
		{Rel: "work/api", Cloned: true},
		{Rel: "notes", Cloned: true, PushNote: "no local main branch to push first"},
		{Rel: "scratch", Err: "git clone failed"},
	})
	if n != 1 {
		t.Errorf("failure count = %d, want 1", n)
	}
	s := out.String()
	for _, want := range []string{"cloned 2", "notes", "no local main branch", "scratch", "git clone failed"} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q:\n%s", want, s)
		}
	}
}

func TestRenderCloneResultAlwaysSaysSomething(t *testing.T) {
	var out strings.Builder
	if n := setup.RenderCloneResult(&out, "peerbox", nil); n != 0 {
		t.Errorf("failure count = %d, want 0", n)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("say the outcome out loud; silence reads as 'it did not try'")
	}
}

func TestRenderRepoChecksDoesNotStopForAClonableRepo(t *testing.T) {
	// A missing repo install is about to clone is news, but it is not a reason
	// to ask the user whether to continue.
	var out strings.Builder
	n := setup.RenderRepoChecks(&out, "peerbox", "/home/peer/code", []setup.RepoCheck{
		{Rel: "notes", State: setup.RepoPresent},
		missing("work/api", "git@github.com:me/api.git"),
	})
	if n != 0 {
		t.Errorf("mismatch count = %d, want 0: the clone stage handles this one", n)
	}
	if s := out.String(); !strings.Contains(s, "work/api") || !strings.Contains(s, "clone") {
		t.Errorf("say that it will be cloned:\n%s", s)
	}
}

func TestRenderRepoChecksStillStopsForWhatCloningCannotFix(t *testing.T) {
	var out strings.Builder
	n := setup.RenderRepoChecks(&out, "peerbox", "/home/peer/code", []setup.RepoCheck{
		missing("work/api", "git@github.com:me/api.git"),
		{Rel: "scratch", State: setup.RepoNotAGitRepo},
	})
	if n != 1 {
		t.Errorf("mismatch count = %d, want 1", n)
	}
	if s := out.String(); !strings.Contains(s, "scratch") {
		t.Errorf("report missing scratch:\n%s", s)
	}
}
