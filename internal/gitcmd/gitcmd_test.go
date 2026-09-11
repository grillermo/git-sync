package gitcmd_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestRunReturnsTrimmedOutput(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	out, err := gitcmd.Run(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if out != "main" {
		t.Errorf("branch = %q, want %q (output should be trimmed)", out, "main")
	}
}

func TestRunErrorIncludesGitsStderr(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	_, err := gitcmd.Run(repo, "checkout", "no-such-branch")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no-such-branch") {
		t.Errorf("error %q should quote what git said", err)
	}
}

func TestIsDirty(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")

	dirty, err := gitcmd.IsDirty(repo)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Error("a fresh clone should be clean")
	}

	sb.Dirty(repo)
	dirty, err = gitcmd.IsDirty(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Error("IsDirty should see an untracked file, not just tracked edits")
	}
}

func TestCurrentBranchDetachedHead(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "checkout", "-q", "--detach")

	_, err := gitcmd.CurrentBranch(repo)
	if !gitcmd.IsDetachedHead(err) {
		t.Errorf("err = %v, want a detached-head error", err)
	}
}

func TestHasRemoteBranch(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	if !gitcmd.HasRemoteBranch(repo, "origin", "main") {
		t.Error("main was pushed, so origin/main exists")
	}
	if gitcmd.HasRemoteBranch(repo, "origin", "orphan") {
		t.Error("a branch never pushed has no remote counterpart")
	}
}

func TestResolveRemotePrefersGithubOverOrigin(t *testing.T) {
	// The whole sync goes through this remote, so when a repo has both, the
	// two machines must pick the same one.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "remote", "add", "github", "https://example.invalid/proj.git")

	got, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "github" {
		t.Errorf("ResolveRemote = %q, want %q", got, "github")
	}
}

func TestResolveRemoteFallsBackToOrigin(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj") // origin only
	got, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "origin" {
		t.Errorf("ResolveRemote = %q, want %q", got, "origin")
	}
}

func TestResolveRemoteAcceptsASingleOddlyNamedRemote(t *testing.T) {
	// One remote and no ambiguity: refusing to sync would be pedantic.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "remote", "rename", "origin", "gitlab")

	got, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "gitlab" {
		t.Errorf("ResolveRemote = %q, want the repo's only remote", got)
	}
}

func TestResolveRemoteIsAmbiguousWithSeveralUnlistedRemotes(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.Git(repo, "remote", "rename", "origin", "gitlab")
	sb.Git(repo, "remote", "add", "backup", "https://example.invalid/b.git")

	if _, err := gitcmd.ResolveRemote(repo, []string{"github", "origin"}); err == nil {
		t.Error("two unlisted remotes is a guess we must not make")
	}
}

func TestResolveRemoteWithNoRemotes(t *testing.T) {
	sb := testutil.NewSandbox(t)
	local := filepath.Join(sb.BaseDir, "local-only")
	testutil.MkdirAll(t, local)
	sb.Git(sb.Home, "init", "-q", local)

	_, err := gitcmd.ResolveRemote(local, []string{"github", "origin"})
	if !gitcmd.IsNoRemote(err) {
		t.Errorf("err = %v, want a no-remote error: this repo can never sync", err)
	}
}

func TestPushNamesTheRemoteAndBranch(t *testing.T) {
	// Not plain `git push`: the branch may track origin while the shared
	// remote is github.
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	testutil.Commit(t, sb, repo, "second")

	if _, err := gitcmd.Push(repo, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if out := sb.Git(repo, "log", "--oneline", "origin/main"); !strings.Contains(out, "second") {
		t.Errorf("commit did not reach origin/main:\n%s", out)
	}
}

func TestFastForwardOntoTheNamedRemote(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sb.PeerClone("proj")
	sb.PeerCommit("proj", "from-peer")

	if err := gitcmd.Fetch(repo, "origin"); err != nil {
		t.Fatal(err)
	}
	if err := gitcmd.FastForward(repo, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if out := sb.Git(repo, "log", "--oneline"); !strings.Contains(out, "from-peer") {
		t.Errorf("did not fast-forward onto origin/main:\n%s", out)
	}
}

func TestToplevel(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")
	sub := filepath.Join(repo, "deep", "nested")
	testutil.MkdirAll(t, sub)

	got, err := gitcmd.Toplevel(sub)
	if err != nil {
		t.Fatal(err)
	}
	// macOS /var -> /private/var, so compare resolved paths.
	if !testutil.SamePath(got, repo) {
		t.Errorf("Toplevel(%q) = %q, want %q", sub, got, repo)
	}
}

func TestAheadBehindCountsBothDirections(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("proj")

	ahead, behind, err := gitcmd.AheadBehind(repo, "origin", "main")
	if err != nil || ahead != 0 || behind != 0 {
		t.Fatalf("fresh clone: got %d/%d, %v; want 0/0, nil", ahead, behind, err)
	}

	// One commit here that origin does not have.
	testutil.Commit(t, sb, repo, "local only")
	ahead, behind, err = gitcmd.AheadBehind(repo, "origin", "main")
	if err != nil || ahead != 1 || behind != 0 {
		t.Fatalf("after a local commit: got %d/%d, %v; want 1/0, nil", ahead, behind, err)
	}

	// And one on origin that we do not have, putting the two on diverged
	// histories - the case a fast-forward must refuse.
	sb.PeerClone("proj")
	sb.PeerCommit("proj", "remote only")
	if err := gitcmd.Fetch(repo, "origin"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ahead, behind, err = gitcmd.AheadBehind(repo, "origin", "main")
	if err != nil || ahead != 1 || behind != 1 {
		t.Fatalf("diverged: got %d/%d, %v; want 1/1, nil", ahead, behind, err)
	}
}

func TestRunMarksItsGitCommandsAsInternal(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	// A hook that fails unless it is told this is git-sync's own git call.
	hooks := filepath.Join(sb.Home, "hooks")
	testutil.MkdirAll(t, hooks)
	testutil.WriteFileIn(t, hooks, "pre-commit",
		"#!/bin/sh\n[ -n \"$GITSYNC_INTERNAL\" ] || exit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb.Git(repo, "config", "core.hooksPath", hooks)

	testutil.AppendFileIn(t, repo, "README.md", "x\n")
	if _, err := gitcmd.Run(repo, "commit", "-am", "internal"); err != nil {
		t.Fatalf("gitcmd.Run must set GITSYNC_INTERNAL: %v", err)
	}
}

func TestSummaryKeepsTheReasonAPushWasRejected(t *testing.T) {
	// Exactly what `git push` writes when the remote has moved on. The first
	// line is the transport banner, so taking it alone drops the diagnosis -
	// which is the bug Summary exists to fix.
	err := errors.New("git push github main: exit status 1: " +
		"To github.com:grillermo/awh.git\n" +
		" ! [rejected]        main -> main (fetch first)\n" +
		"error: failed to push some refs to 'github.com:grillermo/awh.git'\n" +
		"hint: Updates were rejected because the remote contains work that you do not\n" +
		"hint: have locally. This is usually caused by another repository pushing to\n" +
		"hint: the same ref.")

	got := gitcmd.Summary(err)

	if !strings.Contains(got, "[rejected]") {
		t.Errorf("Summary dropped the rejection reason: %q", got)
	}
	if !strings.Contains(got, "fetch first") {
		t.Errorf("Summary dropped the cause: %q", got)
	}
	if strings.Contains(got, "hint:") {
		t.Errorf("Summary kept git's advice block: %q", got)
	}
	if strings.Contains(got, "To github.com") {
		t.Errorf("Summary kept the transport banner: %q", got)
	}
}

func TestSummaryKeepsFatalForAnOrdinaryFailure(t *testing.T) {
	err := errors.New("git rev-parse HEAD: exit status 128: " +
		"fatal: ambiguous argument 'HEAD': unknown revision")
	if got := gitcmd.Summary(err); !strings.Contains(got, "fatal: ambiguous argument") {
		t.Errorf("Summary lost the fatal line: %q", got)
	}
}

func TestSummaryFallsBackToTheFirstLineWhenNothingMatches(t *testing.T) {
	// Unfamiliar output must never be reduced to nothing: an empty message in
	// the log is strictly worse than a possibly-irrelevant one.
	err := errors.New("git something: exit status 1: weird unprefixed output\nsecond line")
	got := gitcmd.Summary(err)
	if got == "" {
		t.Fatal("Summary returned empty for unrecognised output")
	}
	if strings.Contains(got, "second line") {
		t.Errorf("fallback should be the first line only, got %q", got)
	}
}

func TestSummaryOfNilIsEmpty(t *testing.T) {
	if got := gitcmd.Summary(nil); got != "" {
		t.Errorf("Summary(nil) = %q, want empty", got)
	}
}

func TestSummaryKeepsALineOneDiagnosisEvenWhenALaterLineMatches(t *testing.T) {
	// Run formats as "git <args>: <exit>: <git's first output line>", so when
	// the diagnosis is on line 1 it is *embedded*, not at the start of the
	// line. A prefix-only scan would skip it, find the later "warning:", and
	// return a non-empty result with the real error silently dropped - worse
	// than the truncation this replaces, because it looks like it worked.
	err := errors.New("git merge github/main: exit status 1: fatal: refusing to merge unrelated histories\n" +
		"warning: some other thing")

	got := gitcmd.Summary(err)

	if !strings.Contains(got, "refusing to merge unrelated histories") {
		t.Errorf("Summary dropped the line-one diagnosis: %q", got)
	}
}
