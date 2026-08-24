package main

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestConfirmQuitsOnQ(t *testing.T) {
	if confirm(io.Discard, strings.NewReader("q\n"), "?") {
		t.Error("q should quit the wizard")
	}
}

func TestConfirmContinuesOnEnter(t *testing.T) {
	if !confirm(io.Discard, strings.NewReader("\n"), "?") {
		t.Error("a bare enter should continue")
	}
}

func TestConfirmQuitsOnEOF(t *testing.T) {
	// Ctrl-D is a quit, not a blank continue.
	if confirm(io.Discard, strings.NewReader(""), "?") {
		t.Error("EOF should quit")
	}
}

func TestCfgRemotesReflectsTheSavedConfig(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithRemotes(t, sb, "peer", "user", []string{"gitlab"})

	if got := cfgRemotes(); !reflect.DeepEqual(got, []string{"gitlab"}) {
		t.Errorf("cfgRemotes() = %v, want [gitlab]", got)
	}
}

func TestCfgRemotesIsNilWithNoSavedConfig(t *testing.T) {
	testutil.NewSandbox(t)

	if got := cfgRemotes(); got != nil {
		t.Errorf("cfgRemotes() = %v, want nil with nothing saved", got)
	}
}

// TestRepoWantsHonoursANonDefaultRemotePreference guards the install wizard's
// promise that the counterpart check (repoCheckScript's doc comment: "the
// same preference order this machine uses") really does use the saved
// remote_names, not config.DefaultRemoteNames. A repo with both "origin" and
// a preferred "gitlab" remote pointing at different URLs makes the two
// preference orders disagree, so threading cfgRemotes() through matters.
func TestRepoWantsHonoursANonDefaultRemotePreference(t *testing.T) {
	sb := testutil.NewSandbox(t)
	work := sb.MakeRepo("proj") // remote "origin"
	gitlabBare := sb.AddRemote(t, work, "gitlab", "proj")
	testutil.SaveConfigWithRemotes(t, sb, "peer", "user", []string{"gitlab", "origin"})

	// This is exactly how cmdInstall builds the cfg passed to repoWants.
	cfg := config.Config{BaseDir: sb.BaseDir, RemoteNames: cfgRemotes()}
	got := repoWants(cfg, []string{"proj"})

	if len(got) != 1 {
		t.Fatalf("repoWants returned %d entries, want 1: %+v", len(got), got)
	}
	if got[0].RemoteURL != gitlabBare {
		t.Errorf("RemoteURL = %q, want the preferred gitlab remote %q "+
			"(the saved remote_names preference was not honoured)",
			got[0].RemoteURL, gitlabBare)
	}

	// Contrast: without threading the saved preference through (the bug this
	// guards against), resolution falls back to config.DefaultRemoteNames and
	// picks "origin" instead - a different remote from the one push/receive
	// actually use.
	defaultCfg := config.Config{BaseDir: sb.BaseDir}
	gotDefault := repoWants(defaultCfg, []string{"proj"})
	if len(gotDefault) != 1 || gotDefault[0].RemoteURL == gitlabBare {
		t.Fatalf("test fixture invalid: expected the default preference to resolve "+
			"a different remote than gitlab, got %+v", gotDefault)
	}
}
