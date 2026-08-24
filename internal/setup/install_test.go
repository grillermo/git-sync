package setup_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestInstallCreatesTheRuntimeLayout(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	var out strings.Builder
	err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, PeerHost: "peer.example", PeerUser: "tester",
		Self: self, Out: &out,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, p := range []string{config.BinPath(), filepath.Join(config.HooksDir(), "post-commit")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("%s missing: %v", p, err)
			continue
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable (mode %v)", p, fi.Mode())
		}
	}
}

func TestInstallSetsGlobalHooksPath(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	_ = setup.Install(setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard})

	got := strings.TrimSpace(sb.Git(sb.Home, "config", "--global", "core.hooksPath"))
	if got != config.HooksDir() {
		t.Errorf("core.hooksPath = %q, want %q", got, config.HooksDir())
	}
}

func TestInstallRejectsAMissingBaseDir(t *testing.T) {
	sb := testutil.NewSandbox(t)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	err := setup.Install(setup.Options{
		BaseDir: filepath.Join(sb.Home, "nope"), PeerHost: "p", PeerUser: "u",
		Self: self, Out: io.Discard,
	})
	if err == nil {
		t.Error("Install should refuse a base_dir that does not exist")
	}
}

func TestInstallStoresAnAbsoluteBaseDir(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	// A relative path must not be stored: the hook runs from arbitrary cwds.
	testutil.Chdir(t, sb.Home)
	_ = setup.Install(setup.Options{BaseDir: "code", PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.BaseDir) {
		t.Errorf("base_dir = %q, want an absolute path", cfg.BaseDir)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	opts := setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard}

	if err := setup.Install(opts); err != nil {
		t.Fatal(err)
	}
	if err := setup.Install(opts); err != nil {
		t.Fatalf("second install failed: %v", err)
	}
}

func TestInstallUpdatesTheInstalledBinary(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\necho v2\n")
	_ = os.MkdirAll(filepath.Dir(config.BinPath()), 0o755)
	_ = os.WriteFile(config.BinPath(), []byte("stale"), 0o755)

	_ = setup.Install(setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard})

	b, _ := os.ReadFile(config.BinPath())
	if string(b) == "stale" {
		t.Error("re-installing should refresh the binary")
	}
}

func TestInstallPreservesActivityHistory(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	_ = activity.Append(activity.Event{Repo: "old/repo", Op: activity.OpPush, Status: activity.StatusOK})
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	_ = setup.Install(setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard})

	events, _ := activity.Read()
	if len(events) != 1 {
		t.Error("installing must not wipe existing activity history")
	}
}

func TestInstallRecordsTheChosenAllowlist(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	_ = setup.Install(setup.Options{
		BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self,
		Repos: []string{"work/api", "notes"}, NoPeer: true, Out: io.Discard,
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repos) != 2 {
		t.Fatalf("Repos = %v, want the two selected", cfg.Repos)
	}
	if !cfg.IsSelected("work/api") || !cfg.IsSelected("notes") {
		t.Errorf("Repos = %v, want both selected", cfg.Repos)
	}
}

func TestInstallWithNoReposSelectedSyncsNothing(t *testing.T) {
	// Ticking nothing is a legitimate choice, and must not be read as "all".
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")

	if err := setup.Install(setup.Options{
		BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self,
		Repos: nil, NoPeer: true, Out: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.IsSelected("anything") {
		t.Error("an empty selection must sync nothing")
	}
}

func TestInstallReplacesTheAllowlistOnRerun(t *testing.T) {
	// The picker returns the full desired set, so a re-run is a replacement,
	// not a merge - otherwise unticking a repo could never remove it.
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	base := setup.Options{
		BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self,
		NoPeer: true, Out: io.Discard,
	}
	base.Repos = []string{"a", "b"}
	_ = setup.Install(base)
	base.Repos = []string{"a"}
	_ = setup.Install(base)

	cfg, _ := config.Load()
	if len(cfg.Repos) != 1 || cfg.Repos[0] != "a" {
		t.Errorf("Repos = %v, want just [a] - unticking must remove", cfg.Repos)
	}
}

func TestUninstallRemovesTheHookButKeepsHistory(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	_ = setup.Install(setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard})
	_ = activity.Append(activity.Event{Repo: "a", Op: activity.OpPush, Status: activity.StatusOK})

	if err := setup.Uninstall(false, io.Discard); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(filepath.Join(config.HooksDir(), "post-commit")); !os.IsNotExist(err) {
		t.Error("the hook should be gone")
	}
	if _, err := os.Stat(config.AskpassPath()); !os.IsNotExist(err) {
		t.Error("the askpass shim should be gone")
	}
	out, _ := exec.Command("git", "config", "--global", "core.hooksPath").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("core.hooksPath should be unset, got %q", out)
	}
	// History survives, so `git-sync report` still works afterwards.
	events, _ := activity.Read()
	if len(events) != 1 {
		t.Error("uninstall should keep activity history by default")
	}
}

func TestUninstallLeavesAForeignHooksPathAlone(t *testing.T) {
	sb := testutil.NewSandbox(t)
	// Someone else's hooks path - Husky, say. Not ours to remove.
	sb.Git(sb.Home, "config", "--global", "core.hooksPath", "/opt/husky/hooks")

	if err := setup.Uninstall(false, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(sb.Git(sb.Home, "config", "--global", "core.hooksPath"))
	if got != "/opt/husky/hooks" {
		t.Errorf("core.hooksPath = %q; uninstall must not clobber another tool's setting", got)
	}
}

func TestUninstallPurgeRemovesEverything(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	_ = setup.Install(setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard})
	_ = activity.Append(activity.Event{Repo: "a", Op: activity.OpPush, Status: activity.StatusOK})

	if err := setup.Uninstall(true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.Home()); !os.IsNotExist(err) {
		t.Error("--purge should remove the whole gitsync home")
	}
}

func TestUninstallOnAFreshMachineIsNotAnError(t *testing.T) {
	testutil.NewSandbox(t)
	if err := setup.Uninstall(false, io.Discard); err != nil {
		t.Errorf("uninstalling when nothing is installed should be a no-op: %v", err)
	}
}

func TestInstallLeavesNoTempFilesBehind(t *testing.T) {
	// The binary, hook shim, and askpass shim are all written via
	// temp-file-then-rename; a successful install must not leave the .tmp
	// siblings lying around.
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	if err := setup.Install(setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		config.BinPath() + ".tmp",
		filepath.Join(config.HooksDir(), "post-commit") + ".tmp",
		config.AskpassPath() + ".tmp",
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("temp file left behind: %s", p)
		}
	}
}

func TestHookShimInvokesTheInstalledBinary(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	marker := filepath.Join(sb.Home, "hook-ran")
	self := testutil.WriteScript(t, sb, "git-sync-fake",
		"#!/bin/sh\necho \"$@\" > "+marker+"\n")
	_ = setup.Install(setup.Options{BaseDir: sb.BaseDir, PeerHost: "p", PeerUser: "u", Self: self, Out: io.Discard})

	cmd := exec.Command(filepath.Join(config.HooksDir(), "post-commit"))
	cmd.Dir = sb.Home
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the shim: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the shim did not invoke the binary: %v", err)
	}
	if !strings.Contains(string(b), "hook post-commit") {
		t.Errorf("shim called the binary with %q, want 'hook post-commit'", b)
	}
}
