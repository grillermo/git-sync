package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/setup"
)

// cmdResync re-runs install's checks and remediation against the repos already
// in the config, without the picker and without asking for a peer again.
//
// Everything install does after the picker is idempotent by design, so this is
// install minus the choosing: verify the peer's copies, re-lay the local hook
// and re-provision the peer, clone the repos the peer is missing, and level
// both machines with the shared remote. It exists because those stages are the
// ones that go stale - a repo cloned by hand last week, a peer that was down at
// install time, an unpushed commit that quietly turned every later sync into a
// warning - and re-running `install` to fix them means walking the picker again
// and risks re-answering it wrong.
func cmdResync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	only := fs.String("repos", "", "comma-separated subset of the synced repos to check; default all")
	noProvision := fs.Bool("no-provision", false,
		"do not re-write the local hook or re-provision the peer; only check and level the repos")
	noInitialSync := fs.Bool("no-initial-sync", false,
		"do not push/fast-forward the repos level with their remotes")
	noCloneMissing := fs.Bool("no-clone-missing", false,
		"do not clone repos the peer does not have yet")
	peerBaseDir := fs.String("peer-base-dir", "", "the peer's sync root (default: same path relative to $HOME)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: git-sync resync [flags]")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		if config.IsNotInstalled(err) {
			fmt.Fprintln(stderr, "git-sync is not installed here; run 'git-sync install <base_dir>' first")
			return 1
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	if cfg.PeerHost == "" || cfg.PeerUser == "" {
		fmt.Fprintln(stderr, "no peer in the config; run 'git-sync install <base_dir>' first")
		return 1
	}

	repos, err := resyncRepos(cfg, *only)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(repos) == 0 {
		fmt.Fprintln(stdout, "no repos are being synced; run 'git-sync install <base_dir>' to pick some")
		return 0
	}

	// Stage "connect", same as install: settle password auth before anything
	// tries to ssh, so a peer that stopped accepting the stored password says
	// so once here instead of failing every later stage.
	var stdin io.Reader
	if isTTY(stdout) {
		stdin = os.Stdin
	}
	if err := setup.EnsureAuth(setup.Target(cfg), stdin, stdout); err != nil {
		if setup.IsPeerUnreachable(err) {
			fmt.Fprintf(stderr, "could not reach %s (%v); continuing\n", cfg.PeerHost, err)
		} else {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	// Stage "verify".
	fmt.Fprintf(stdout, "checking %d repos on %s\n", len(repos), cfg.PeerHost)
	checks, ok := checkPeer(cfg, *peerBaseDir, repoWants(cfg, repos), stdout, stderr)
	if !ok {
		fmt.Fprintln(stdout, "cancelled; nothing was changed and the peer was not touched")
		return 0
	}

	// Stage "install". Re-laying the binary, the shims and core.hooksPath is
	// what repairs a half-installed machine; provisioning the peer is what
	// finally sets one up that was unreachable when install ran. The allowlist
	// passed here is the whole saved one, never the --repos subset: a filtered
	// check must not quietly shrink what this machine syncs.
	if !*noProvision {
		if err := setup.Install(setup.Options{
			BaseDir: cfg.BaseDir, PeerHost: cfg.PeerHost, PeerUser: cfg.PeerUser,
			Repos: cfg.Repos, PeerBaseDir: *peerBaseDir, Out: stdout,
		}); err != nil {
			fmt.Fprintln(stderr, "resync failed:", err)
			return 1
		}
	}

	// Stages "clone" and "level", in install's order and for install's
	// reasons: a freshly cloned repo is then measured like any other, and both
	// happen once the hook is armed.
	if !*noCloneMissing {
		cloneMissingRepos(cfg, *peerBaseDir, checks, stdout, stderr)
	}
	if !*noInitialSync {
		levelRepos(cfg, *peerBaseDir, repos, stdout, stderr)
	}
	return 0
}

// resyncRepos resolves which of the config's repos to work on. An empty filter
// means all of them; a filter naming something that is not synced is an error
// rather than a silent no-op, since the likely cause is a typo and the likely
// reaction to "nothing happened" is to believe the repo is fine.
func resyncRepos(cfg config.Config, only string) ([]string, error) {
	if strings.TrimSpace(only) == "" {
		return cfg.Repos, nil
	}
	synced := make(map[string]bool, len(cfg.Repos))
	for _, rel := range cfg.Repos {
		synced[rel] = true
	}

	var out, unknown []string
	for _, rel := range strings.Split(only, ",") {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		if !synced[rel] {
			unknown = append(unknown, rel)
			continue
		}
		out = append(out, rel)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("not synced by git-sync: %s (run 'git-sync install %s' to add them)",
			strings.Join(unknown, ", "), cfg.BaseDir)
	}
	return out, nil
}
