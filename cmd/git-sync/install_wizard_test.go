package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/status"
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

func TestPeersFromSkipsAHostileDiscoveredHostname(t *testing.T) {
	// Hostnames come off the network via mDNS and end up in remote shell
	// commands: a bad one is dropped, not provisioned, and not fatal.
	var out bytes.Buffer
	peers, ok, err := peersFrom([]string{"studio.local", "evil;rm -rf ~.local"}, "g", &out)
	if err != nil || !ok {
		t.Fatalf("peersFrom = %v, %v", ok, err)
	}
	if want := []config.Peer{{Host: "studio.local", User: "g"}}; !reflect.DeepEqual(peers, want) {
		t.Errorf("peers = %+v, want %+v", peers, want)
	}
	if !strings.Contains(out.String(), "skipping") {
		t.Errorf("the dropped host must be reported:\n%s", out.String())
	}
}

func TestPeersFromRejectsABadUsername(t *testing.T) {
	if _, ok, err := peersFrom([]string{"studio.local"}, "g h", io.Discard); err == nil || ok {
		t.Errorf("ok=%v err=%v, want the typo refused", ok, err)
	}
}

func TestPendingSelectionIsKeyedByBaseDir(t *testing.T) {
	testutil.NewSandbox(t)
	if err := savePending("/code", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got := loadPending("/code"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("loadPending = %v", got)
	}
	if got := loadPending("/elsewhere"); got != nil {
		t.Errorf("a selection for another base_dir must not be reused: %v", got)
	}
	clearPending()
	if got := loadPending("/code"); got != nil {
		t.Errorf("after clear: %v", got)
	}
}

func TestChooseReposReusesAPendingSelectionWithoutThePicker(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("a")
	_ = savePending(sb.BaseDir, []string{"a"})

	// No terminal: without the saved selection this would be an error.
	got, err := chooseRepos(sb.BaseDir, false, "", io.Discard, io.Discard)
	if err != nil || !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("chooseRepos = %v, %v; want the saved [a]", got, err)
	}
}

func TestInstallForgetsThePendingSelectionOnceEveryMachinePairs(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	sb.StubSSHScripted(map[string]string{"*uname*": "Darwin arm64", "*HOME*": "/home/t"}, 0)
	_ = savePending(sb.BaseDir, []string{"group/proj"})

	if code := run([]string{"install", "--peer", "t@b.local", "--no-initial-sync", sb.BaseDir},
		io.Discard, io.Discard); code != 0 {
		t.Fatalf("install = %d", code)
	}
	if loadPending(sb.BaseDir) != nil {
		t.Error("a fully paired install must drop the saved selection")
	}
}

func TestInstallKeepsThePendingSelectionWhileAMachineIsUnreachable(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/proj")
	sb.StubSSHFailing(255, "Host key verification failed.")
	_ = savePending(sb.BaseDir, []string{"group/proj"})

	var errOut bytes.Buffer
	run([]string{"install", "--peer", "t@b.local", "--no-initial-sync", sb.BaseDir}, io.Discard, &errOut)
	if loadPending(sb.BaseDir) == nil {
		t.Error("the selection must survive until the mesh pairs")
	}
	if !strings.Contains(errOut.String(), "ssh t@b.local true") {
		t.Errorf("the unreachable machine's fix must be spelled out:\n%s", errOut.String())
	}
}

func TestInstallWithNoBaseDirAddsJustTheCurrentRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("group/old")
	here := sb.MakeRepo("group/proj")
	sb.MakeRepo("group/other")
	testutil.SaveConfigWithRepos(t, sb, "b.local", "t", []string{"group/old"})
	sb.StubSSHScripted(map[string]string{"*uname*": "Darwin arm64", "*HOME*": "/home/t"}, 0)
	testutil.Chdir(t, here)

	var errOut bytes.Buffer
	if code := run([]string{"install", "--no-initial-sync"}, io.Discard, &errOut); code != 0 {
		t.Fatalf("install = %d, want 0; stderr: %s", code, errOut.String())
	}
	cfg, _ := config.Load()
	if !testutil.SamePath(cfg.BaseDir, sb.BaseDir) {
		t.Errorf("base_dir = %q, want the existing %q", cfg.BaseDir, sb.BaseDir)
	}
	if want := []string{"group/old", "group/proj"}; !reflect.DeepEqual(cfg.Repos, want) {
		t.Errorf("repos = %v, want %v (the current repo added, nothing scanned)", cfg.Repos, want)
	}
}

func TestInstallWithNoBaseDirDefaultsToTheRepoParent(t *testing.T) {
	sb := testutil.NewSandbox(t)
	here := sb.MakeRepo("group/proj")
	testutil.Chdir(t, here)

	var errOut bytes.Buffer
	if code := run([]string{"install", "--no-peer"}, io.Discard, &errOut); code != 0 {
		t.Fatalf("install = %d, want 0; stderr: %s", code, errOut.String())
	}
	cfg, _ := config.Load()
	if want := filepath.Join(sb.BaseDir, "group"); !testutil.SamePath(cfg.BaseDir, want) {
		t.Errorf("base_dir = %q, want the repo's parent %q", cfg.BaseDir, want)
	}
	if want := []string{"proj"}; !reflect.DeepEqual(cfg.Repos, want) {
		t.Errorf("repos = %v, want %v", cfg.Repos, want)
	}
}

func TestInstallWithNoBaseDirRefusesARepoOutsideTheInstalledBase(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfigWithRepos(t, sb, "b.local", "t", nil)
	outside := t.TempDir()
	sb.Git(outside, "init", "-q")
	testutil.Chdir(t, outside)

	var errOut bytes.Buffer
	if code := run([]string{"install", "--no-peer"}, io.Discard, &errOut); code != 2 {
		t.Fatalf("install = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "not under base_dir") {
		t.Errorf("stderr = %q, want it to say the repo is not under base_dir", errOut.String())
	}
}

func TestInstallWithNoBaseDirOutsideARepoIsAUsageError(t *testing.T) {
	testutil.NewSandbox(t)
	testutil.Chdir(t, t.TempDir())

	if code := run([]string{"install", "--no-peer"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("install = %d, want 2", code)
	}
}

func TestUninstallFolderStopsSyncingJustThatRepoMeshWide(t *testing.T) {
	sb := testutil.NewSandbox(t)
	work := sb.MakeRepo("group/proj")
	sb.MakeRepo("other")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{
		{Host: "b.local", User: "t"}, {Host: "c.local", User: "t"},
	}, nil)
	sb.StubSSH(0)

	// A subfolder of the repo names the repo.
	sub := filepath.Join(work, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := run([]string{"uninstall", sub}, &out, &errBuf); code != 0 {
		t.Fatalf("uninstall <folder> = %d: %s", code, errBuf.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("git-sync itself must stay installed: %v", err)
	}
	if cfg.IsSelected("group/proj") || !cfg.IsSelected("other") {
		t.Errorf("repos = %v, want only [other]", cfg.Repos)
	}
	calls := sb.SSHCalls()
	for _, host := range []string{"b.local", "c.local"} {
		if !strings.Contains(calls, host) {
			t.Errorf("%s was never told; calls:\n%s", host, calls)
		}
	}
	if !strings.Contains(calls, "uninstall --local --repo group/proj") {
		t.Errorf("peers must get the --repo machine form:\n%s", calls)
	}
}

func TestUninstallFolderDropsTheRepoFromStatus(t *testing.T) {
	sb := testutil.NewSandbox(t)
	work := sb.MakeRepo("proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "b.local", User: "t"}}, nil)
	_ = activity.Append(activity.Event{Repo: "proj", Op: activity.OpPush, Status: activity.StatusError, Msg: "rejected"})
	sb.StubSSH(0)

	if code := run([]string{"uninstall", work}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("uninstall <folder> = %d", code)
	}
	var out bytes.Buffer
	if code := run([]string{"status", "--json"}, &out, io.Discard); code != 0 {
		t.Fatalf("status --json = %d", code)
	}
	var s status.Snapshot
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("not one JSON snapshot: %v\n%s", err, out.String())
	}
	if len(s.Repos) != 0 || s.Problems != 0 {
		t.Errorf("the menu bar app must no longer see the repo: %+v", s)
	}
}

func TestUninstallRepoLocalIsIdempotent(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("proj")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "b.local", User: "t"}}, nil)
	sb.StubSSH(0)

	for i := 0; i < 2; i++ {
		if code := run([]string{"uninstall", "--local", "--repo", "proj"}, io.Discard, io.Discard); code != 0 {
			t.Fatalf("run %d: uninstall --local --repo = %d", i, code)
		}
	}
	if strings.Contains(sb.SSHCalls(), "b.local") {
		t.Errorf("--local must not ssh anywhere:\n%s", sb.SSHCalls())
	}
}

func TestUninstallFolderNotSyncedFails(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("proj")
	stray := sb.MakeRepo("stray")
	testutil.SaveConfigWithPeers(t, sb, []config.Peer{{Host: "b.local", User: "t"}}, []string{"proj"})
	sb.StubSSH(0)

	var errBuf bytes.Buffer
	if code := run([]string{"uninstall", stray}, io.Discard, &errBuf); code != 1 {
		t.Errorf("uninstall <unsynced folder> = %d, want 1: %s", code, errBuf.String())
	}
	if sb.SSHCalls() != "" {
		t.Errorf("nothing should be sent to peers:\n%s", sb.SSHCalls())
	}
}

func TestUninstallFolderRefusesPurge(t *testing.T) {
	sb := testutil.NewSandbox(t)
	work := sb.MakeRepo("proj")
	testutil.SaveConfigWithPeers(t, sb, nil, nil)

	if code := run([]string{"uninstall", "--purge", work}, io.Discard, io.Discard); code != 2 {
		t.Errorf("uninstall --purge <folder> = %d, want 2", code)
	}
	if _, err := config.Load(); err != nil {
		t.Errorf("config must be untouched: %v", err)
	}
}

func TestUpdateMeshNeedsAnInstall(t *testing.T) {
	testutil.NewSandbox(t)
	var errBuf bytes.Buffer
	if code := run([]string{"update-mesh"}, io.Discard, &errBuf); code != 1 ||
		!strings.Contains(errBuf.String(), "git-sync install") {
		t.Errorf("update-mesh with no config = %d: %s", code, errBuf.String())
	}
}
