package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestResyncRefusesWhenNotInstalled(t *testing.T) {
	testutil.NewSandbox(t)

	var out strings.Builder
	code := cmdResync(nil, &out, &out)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "install") {
		t.Errorf("output = %q, want it to point at 'git-sync install'", out.String())
	}
}

func TestResyncRefusesWithNoPeerInTheConfig(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("proj")
	testutil.SaveConfigWithRepos(t, sb, "", "", []string{"proj"})

	var out strings.Builder
	if code := cmdResync(nil, &out, &out); code != 1 {
		t.Fatalf("exit code = %d, want 1 with no peer configured", code)
	}
}

// A --repos filter naming something that is not in the allowlist is a typo, and
// a typo that reported success would leave the user believing a repo had been
// checked when nothing looked at it.
func TestResyncRejectsARepoItDoesNotSync(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("proj")
	testutil.SaveConfigWithRepos(t, sb, "peer", "user", []string{"proj"})
	sb.StubSSH(0)

	var out strings.Builder
	code := cmdResync([]string{"--repos", "proj,other"}, &out, &out)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "other") {
		t.Errorf("output = %q, want it to name the unsynced repo", out.String())
	}
	if n := sb.SSHCalls(); n != "" {
		t.Errorf("resync reached the peer before validating the filter:\n%s", n)
	}
}

func TestResyncReposDefaultsToTheWholeAllowlist(t *testing.T) {
	cfg := config.Config{Repos: []string{"a", "b"}}

	got, err := resyncRepos(cfg, "")
	if err != nil {
		t.Fatalf("resyncRepos: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("resyncRepos = %v, want [a b]", got)
	}
}

func TestResyncReposFiltersToTheNamedSubset(t *testing.T) {
	cfg := config.Config{Repos: []string{"a", "b", "c"}}

	got, err := resyncRepos(cfg, " b , c ")
	if err != nil {
		t.Fatalf("resyncRepos: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("resyncRepos = %v, want [b c]", got)
	}
}

// The filter says which repos to *look at*, never which repos this machine
// syncs. Re-writing the config with the subset would silently un-sync the rest.
func TestResyncKeepsTheFullAllowlistWhenFiltered(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("a")
	sb.MakeRepo("b")
	testutil.SaveConfigWithRepos(t, sb, "peer", "user", []string{"a", "b"})
	// Unreachable peer: every stage degrades to a warning, which is exactly the
	// path where a config rewrite would go unnoticed.
	sb.StubSSHFailing(255, "ssh: connect to host peer port 22: No route to host")

	var out strings.Builder
	if code := cmdResync([]string{"--repos", "a"}, &out, &out); code != 0 {
		t.Fatalf("exit code = %d, want 0 (an unreachable peer is a warning):\n%s", code, out.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading config after resync: %v", err)
	}
	if !reflect.DeepEqual(cfg.Repos, []string{"a", "b"}) {
		t.Errorf("config repos = %v, want [a b] - the --repos filter shrank the allowlist", cfg.Repos)
	}
}

// --no-provision is for "check my repos, leave my machine alone": it must not
// touch the hook, the binary or the peer's copy of them.
func TestResyncNoProvisionLeavesTheInstallAlone(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.MakeRepo("a")
	testutil.SaveConfigWithRepos(t, sb, "peer", "user", []string{"a"})
	sb.StubSSHFailing(255, "ssh: connect to host peer port 22: No route to host")

	var out strings.Builder
	if code := cmdResync([]string{"--no-provision"}, &out, &out); code != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", code, out.String())
	}

	if _, err := os.Stat(config.BinPath()); err == nil {
		t.Error("--no-provision installed the binary")
	}
	if _, err := os.Stat(config.AskpassPath()); err == nil {
		t.Error("--no-provision wrote the askpass shim")
	}
}
