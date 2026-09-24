package main

import (
	"bytes"
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

func TestInstallAcceptsSeveralPeers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	sb.StubSSHScripted(map[string]string{
		"*uname*": "Darwin arm64",
		"*HOME*":  "/home/t",
	}, 0)

	code := run([]string{"install", "--peer", "t@b.local", "--peer", "t@c.local",
		"--all", "--no-initial-sync", sb.BaseDir}, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("install = %d, want 0", code)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.PeerList()) != 2 {
		t.Fatalf("config has %d peers, want 2: %+v", len(cfg.PeerList()), cfg.Peers)
	}
}

func TestInstallParsesAPeerBaseDirSuffix(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	sb.StubSSHScripted(map[string]string{"*uname*": "Darwin arm64", "*HOME*": "/home/t"}, 0)

	if code := run([]string{"install", "--peer", "t@b.local:/srv/code", "--all",
		"--no-initial-sync", sb.BaseDir}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("install = %d, want 0", code)
	}
	cfg, _ := config.Load()
	if cfg.PeerList()[0].BaseDir != "/srv/code" {
		t.Errorf("peer base_dir = %q, want /srv/code", cfg.PeerList()[0].BaseDir)
	}
}

func TestUninstallRemovesEveryMachine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"},
	}, []string{})
	sb.StubSSH(0)

	var out bytes.Buffer
	if code := run([]string{"uninstall"}, &out, io.Discard); code != 0 {
		t.Fatalf("uninstall = %d, want 0", code)
	}
	calls := sb.SSHCalls()
	for _, host := range []string{"b.local", "c.local"} {
		if !strings.Contains(calls, host) {
			t.Errorf("%s was never uninstalled; calls:\n%s", host, calls)
		}
	}
	if !strings.Contains(calls, "uninstall --local") {
		t.Errorf("peers must be uninstalled with --local:\n%s", calls)
	}
}

func TestUninstallReportsAnUnreachableMachine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "gone.local", User: "t"}}, []string{})
	sb.StubSSHFailing(255, "no route to host")

	var out bytes.Buffer
	if code := run([]string{"uninstall"}, &out, io.Discard); code != 0 {
		t.Fatalf("uninstall = %d, want 0 (this machine still uninstalls)", code)
	}
	if !strings.Contains(out.String(), "gone.local") ||
		!strings.Contains(out.String(), "git-sync uninstall") {
		t.Errorf("must tell the user to clean up by hand:\n%s", out.String())
	}
}

func TestUninstallLocalDoesNotTouchPeers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "b.local", User: "t"}}, []string{})
	sb.StubSSH(0)

	if code := run([]string{"uninstall", "--local"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("uninstall --local = %d, want 0", code)
	}
	if strings.Contains(sb.SSHCalls(), "b.local") {
		t.Errorf("--local must not ssh anywhere:\n%s", sb.SSHCalls())
	}
}
