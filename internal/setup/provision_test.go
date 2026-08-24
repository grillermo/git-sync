package setup_test

import (
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

// localUname mimics `uname -sm` for the platform running the test.
func localUname() string {
	return localOS() + " " + unameArch()
}

func localOS() string {
	if runtime.GOOS == "darwin" {
		return "Darwin"
	}
	return "Linux"
}

func unameArch() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return runtime.GOARCH
}

func TestProvisionCopiesBinaryConfigAndHook(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"uname": runtime.GOOS + " " + unameArch(),
		"$HOME": "/home/peer",
	}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	var out strings.Builder
	err := setup.ProvisionPeer(setup.PeerOptions{
		Cfg: config.Config{
			BaseDir: sb.BaseDir, PeerHost: "peer.example", PeerUser: "tester",
			Repos: []string{"notes", "work/api"},
		},
		Self: self, SelfHost: "this-machine", Out: &out,
	})
	if err != nil {
		t.Fatalf("ProvisionPeer: %v", err)
	}

	calls := sb.SSHCalls()
	for _, want := range []string{
		"mkdir -p /home/peer/.gitsync",
		"/home/peer/.gitsync/bin/git-sync",
		"/home/peer/.gitsync/config.toml",
		"/home/peer/.gitsync/hooks/post-commit",
		"core.hooksPath /home/peer/.gitsync/hooks",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("ssh calls missing %q:\n%s", want, calls)
		}
	}
}

func TestProvisionWritesAMirroredConfig(t *testing.T) {
	// The peer's config must point back at this machine and carry the same
	// allowlist, or the pair syncs in one direction only.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"uname": localUname(), "$HOME": "/home/peer"}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	_ = setup.ProvisionPeer(setup.PeerOptions{
		Cfg: config.Config{
			BaseDir: "/Users/me/code", PeerHost: "peer.example", PeerUser: "tester",
			Repos: []string{"notes", "work/api"},
		},
		Self: self, SelfHost: "this-machine", SelfUser: "me", Out: io.Discard,
	})

	written := sb.SSHStdin(t, "config.toml")
	for _, want := range []string{
		`peer_host = "this-machine"`,
		`peer_user = "me"`,
		`"notes"`,
		`"work/api"`,
		`base_dir = "/home/peer/code"`, // same path relative to $HOME
	} {
		if !strings.Contains(written, want) {
			t.Errorf("peer config missing %q:\n%s", want, written)
		}
	}
}

func TestProvisionHonoursPeerBaseDirOverride(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"uname": localUname(), "$HOME": "/home/peer"}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	_ = setup.ProvisionPeer(setup.PeerOptions{
		Cfg:         config.Config{BaseDir: "/Users/me/code", PeerHost: "h", PeerUser: "u"},
		Self:        self,
		PeerBaseDir: "/srv/repos",
		Out:         io.Discard,
	})
	if got := sb.SSHStdin(t, "config.toml"); !strings.Contains(got, `base_dir = "/srv/repos"`) {
		t.Errorf("override ignored:\n%s", got)
	}
}

func TestProvisionRefusesAnArchitectureMismatch(t *testing.T) {
	sb := testutil.NewSandbox(t)
	// The binary is copied verbatim; a mismatched peer could never run it.
	sb.StubSSHScripted(map[string]string{"uname": "Linux x86_64-definitely-not-ours"}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	var out strings.Builder
	err := setup.ProvisionPeer(setup.PeerOptions{
		Cfg: config.Config{PeerHost: "peer.example", PeerUser: "u"}, Self: self, Out: &out,
	})
	if err == nil {
		t.Fatal("expected an error for an architecture mismatch")
	}
	if strings.Contains(sb.SSHCalls(), "config.toml") {
		t.Error("must not write anything to a peer that cannot run the binary")
	}
}

func TestProvisionUnreachablePeerIsReportedNotFatal(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(nil, 255)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	err := setup.ProvisionPeer(setup.PeerOptions{
		Cfg: config.Config{PeerHost: "peer.example", PeerUser: "u"}, Self: self, Out: io.Discard,
	})
	if err == nil {
		t.Fatal("expected an error when the peer is unreachable")
	}
	if !setup.IsPeerUnreachable(err) {
		t.Errorf("err = %v, want it classified as unreachable so Install can carry on", err)
	}
}

func TestInstallSucceedsWhenThePeerIsUnreachable(t *testing.T) {
	// The local half is still correct and useful; re-running later finishes
	// the job. This must not fail the whole install.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(nil, 255)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	var out strings.Builder
	err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, PeerHost: "peer.example", PeerUser: "u",
		Self: self, Repos: []string{"proj"}, Out: &out,
	})
	if err != nil {
		t.Fatalf("Install should survive an unreachable peer: %v", err)
	}
	if !strings.Contains(out.String(), "not provisioned") {
		t.Errorf("output should say the peer was skipped:\n%s", out.String())
	}
	if _, statErr := os.Stat(config.BinPath()); statErr != nil {
		t.Error("the local install should still be complete")
	}
}

func TestInstallNoPeerSkipsProvisioning(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"uname": localUname(), "$HOME": "/home/peer"}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	_ = setup.Install(setup.Options{
		BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u",
		Self: self, Repos: []string{"proj"}, NoPeer: true, Out: io.Discard,
	})
	if calls := sb.SSHCalls(); strings.Contains(calls, "config.toml") {
		t.Errorf("--no-peer must not touch the peer:\n%s", calls)
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"uname": localUname(), "$HOME": "/home/peer"}, 0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	opts := setup.PeerOptions{
		Cfg:  config.Config{BaseDir: "/Users/me/code", PeerHost: "h", PeerUser: "u"},
		Self: self, Out: io.Discard,
	}
	if err := setup.ProvisionPeer(opts); err != nil {
		t.Fatal(err)
	}
	if err := setup.ProvisionPeer(opts); err != nil {
		t.Fatalf("second run failed: %v", err)
	}
}

// wants mirrors what the local scan found: the repo and the remote URL this
// machine syncs it through.
func wants(pairs ...string) []setup.RepoWant {
	var out []setup.RepoWant
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, setup.RepoWant{Rel: pairs[i], RemoteURL: pairs[i+1]})
	}
	return out
}

func TestCheckPeerReposClassifiesEachRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"git-sync-repo-check": "url notes git@github.com:me/notes.git\n" +
			"missing work/api\nnot-a-repo scratch\n",
	}, 0)

	got, err := setup.CheckPeerRepos("tester@peer.example", "/home/peer/code",
		wants("notes", "git@github.com:me/notes.git",
			"work/api", "u2",
			"scratch", "u3"))
	if err != nil {
		t.Fatalf("CheckPeerRepos: %v", err)
	}
	want := []setup.RepoCheck{
		{Rel: "notes", State: setup.RepoPresent},
		{Rel: "work/api", State: setup.RepoMissing},
		{Rel: "scratch", State: setup.RepoNotAGitRepo},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d checks, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Rel != want[i].Rel || got[i].State != want[i].State {
			t.Errorf("check[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCheckPeerReposFlagsADifferentRemote(t *testing.T) {
	// Same path, same name, different repository: the two machines would push
	// and pull past each other forever. This is the failure the whole check
	// exists for, now that the remote is the transport.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"git-sync-repo-check": "url notes git@github.com:someone-else/notes.git\n",
	}, 0)

	got, _ := setup.CheckPeerRepos("u@h", "/home/peer/code",
		wants("notes", "git@github.com:me/notes.git"))

	if len(got) != 1 || got[0].State != setup.RepoOtherRemote {
		t.Fatalf("checks = %+v, want notes flagged as a different remote", got)
	}
	if got[0].PeerRemoteURL != "git@github.com:someone-else/notes.git" {
		t.Errorf("PeerRemoteURL = %q; the report has to show both URLs to be useful",
			got[0].PeerRemoteURL)
	}
}

func TestCheckPeerReposFlagsAPeerCloneWithNoRemote(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"git-sync-repo-check": "no-remote notes\n"}, 0)

	got, _ := setup.CheckPeerRepos("u@h", "/home/peer/code", wants("notes", "u"))
	if len(got) != 1 || got[0].State != setup.RepoNoRemote {
		t.Errorf("checks = %+v, want notes flagged as having no remote", got)
	}
}

func TestCheckPeerReposIgnoresATrailingDotGitAndSlash(t *testing.T) {
	// git@host:me/notes.git and git@host:me/notes are the same remote; do not
	// cry wolf over punctuation.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"git-sync-repo-check": "url notes git@github.com:me/notes/\n",
	}, 0)

	got, _ := setup.CheckPeerRepos("u@h", "/home/peer/code",
		wants("notes", "git@github.com:me/notes.git"))
	if got[0].State != setup.RepoPresent {
		t.Errorf("checks = %+v, want these treated as the same remote", got)
	}
}

func TestCheckPeerReposSendsOneCommandForAllRepos(t *testing.T) {
	// One ssh round trip, not one per repo: a 40-repo install would otherwise
	// take 40 handshakes.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{
		"git-sync-repo-check": "url a u\nurl b u\nurl c u\n",
	}, 0)

	_, _ = setup.CheckPeerRepos("u@h", "/home/peer/code",
		wants("a", "u", "b", "u", "c", "u"))

	if n := strings.Count(sb.SSHCalls(), "git-sync-repo-check"); n != 1 {
		t.Errorf("sent %d check commands, want 1", n)
	}
}

func TestCheckPeerReposAsksAboutTheDotGitDirectory(t *testing.T) {
	// "the directory exists" is not the same as "the repo is cloned there".
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"git-sync-repo-check": "url a u\n"}, 0)

	_, _ = setup.CheckPeerRepos("u@h", "/home/peer/code", wants("a", "u"))

	if !strings.Contains(sb.SSHCalls(), ".git") {
		t.Errorf("the check should test for .git, not just the directory:\n%s", sb.SSHCalls())
	}
}

func TestCheckPeerReposResolvesTheRemoteByTheSameNames(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"git-sync-repo-check": "url a u\n"}, 0)

	_, _ = setup.CheckPeerReposWithRemotes("u@h", "/home/peer/code",
		wants("a", "u"), []string{"github", "origin"})

	calls := sb.SSHCalls()
	for _, want := range []string{"github", "origin", "remote get-url"} {
		if !strings.Contains(calls, want) {
			t.Errorf("the remote command should ask for %q:\n%s", want, calls)
		}
	}
}

func TestCheckPeerReposWithNoReposMakesNoSSHCall(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(nil, 0)

	got, err := setup.CheckPeerRepos("u@h", "/home/peer/code", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no checks and no error", got, err)
	}
	if sb.SSHCalls() != "" {
		t.Errorf("nothing selected means nothing to ask:\n%s", sb.SSHCalls())
	}
}

func TestCheckPeerReposSkipsAQuotedRelpath(t *testing.T) {
	// The relpath is interpolated into a single-quoted remote command, exactly
	// as in push. Report it as unchecked rather than breaking out of the quotes.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"git-sync-repo-check": "url fine u\n"}, 0)

	got, err := setup.CheckPeerRepos("u@h", "/home/peer/code",
		wants("fine", "u", "it's/bad", "u"))
	if err != nil {
		t.Fatal(err)
	}
	var bad setup.RepoCheck
	for _, c := range got {
		if c.Rel == "it's/bad" {
			bad = c
		}
	}
	if bad.State != setup.RepoUnchecked {
		t.Errorf("check for a quoted relpath = %+v, want unchecked", bad)
	}
	if strings.Contains(sb.SSHCalls(), "it's/bad") {
		t.Errorf("a quoted relpath must never reach the remote command:\n%s", sb.SSHCalls())
	}
}

func TestCheckPeerReposUnreachableIsClassified(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(nil, 255)

	_, err := setup.CheckPeerRepos("u@h", "/home/peer/code", wants("a", "u"))
	if !setup.IsPeerUnreachable(err) {
		t.Errorf("err = %v, want it classified as unreachable so install can carry on", err)
	}
}

func TestCheckPeerReposReportsARepoTheAnswerOmitted(t *testing.T) {
	// A truncated or garbled answer must not silently become "present".
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"git-sync-repo-check": "url a u\n"}, 0)

	got, _ := setup.CheckPeerRepos("u@h", "/home/peer/code", wants("a", "u", "b", "u"))
	if len(got) != 2 || got[1].State != setup.RepoUnchecked {
		t.Errorf("checks = %+v, want b reported as unchecked", got)
	}
}

func TestRenderRepoChecksListsOnlyTheMismatches(t *testing.T) {
	var out strings.Builder
	n := setup.RenderRepoChecks(&out, "peerbox", "/home/peer/code", []setup.RepoCheck{
		{Rel: "notes", State: setup.RepoPresent},
		{Rel: "work/api", State: setup.RepoMissing},
		{Rel: "scratch", State: setup.RepoNotAGitRepo},
		{Rel: "gh", State: setup.RepoOtherRemote,
			RemoteURL: "git@github.com:me/gh.git", PeerRemoteURL: "git@github.com:you/gh.git"},
	})
	if n != 3 {
		t.Errorf("mismatch count = %d, want 3", n)
	}
	s := out.String()
	if !strings.Contains(s, "git@github.com:you/gh.git") {
		t.Errorf("a remote mismatch has to show the peer's URL to be actionable:\n%s", s)
	}
	for _, want := range []string{"work/api", "scratch", "peerbox", "/home/peer/code"} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "notes") {
		t.Errorf("a repo present on both machines is not news:\n%s", s)
	}
}

func TestRenderRepoChecksSaysNothingWhenEverythingMatches(t *testing.T) {
	var out strings.Builder
	n := setup.RenderRepoChecks(&out, "peerbox", "/home/peer/code", []setup.RepoCheck{
		{Rel: "notes", State: setup.RepoPresent},
	})
	if n != 0 {
		t.Errorf("mismatch count = %d, want 0", n)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("confirm the happy path out loud; silence reads as 'it did not check'")
	}
}

func TestRenderRepoChecksExplainsTheConsequence(t *testing.T) {
	// A list of names is not actionable. Say what will happen and what to do.
	var out strings.Builder
	setup.RenderRepoChecks(&out, "peerbox", "/home/peer/code",
		[]setup.RepoCheck{{Rel: "work/api", State: setup.RepoMissing}})
	s := out.String()
	if !strings.Contains(s, "will not sync") || !strings.Contains(s, "clone") {
		t.Errorf("report should say it will not sync and that cloning fixes it:\n%s", s)
	}
}
