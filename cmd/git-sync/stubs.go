package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/picker"
	"github.com/grillermo/git-sync/internal/scan"
	"github.com/grillermo/git-sync/internal/secret"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/syncer"
)

func cmdInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	peerHost := fs.String("peer-host", "", "hostname of the other machine")
	peerUser := fs.String("peer-user", "", "username on the other machine")
	all := fs.Bool("all", false, "sync every repo found; skip the picker")
	only := fs.String("repos", "", "comma-separated repos to sync; skips the picker")
	noPeer := fs.Bool("no-peer", false, "do not provision the peer machine")
	selfHost := fs.String("self-host", "", "this machine's hostname, as the peer sees it")
	selfUser := fs.String("self-user", "", "the account the peer should ssh back into")
	peerBaseDir := fs.String("peer-base-dir", "", "the peer's sync root (default: same path relative to $HOME)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: git-sync install [--peer-host H --peer-user U] <base_dir>")
		return 2
	}

	host, user := *peerHost, *peerUser
	if host == "" || user == "" {
		// Fall back to an existing config, then to prompting.
		if cfg, err := config.Load(); err == nil {
			if host == "" {
				host = cfg.PeerHost
			}
			if user == "" {
				user = cfg.PeerUser
			}
		}
	}
	if host == "" {
		host = prompt(stdout, "peer hostname: ")
	}
	if user == "" {
		user = prompt(stdout, "peer username: ")
	}
	if host == "" || user == "" {
		fmt.Fprintln(stderr, "peer host and user are required (--peer-host, --peer-user)")
		return 2
	}

	base, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// Stage "connect": settle password auth with the peer, if it needs one,
	// before picking repos - asking after the user has already ticked forty
	// repos would mean asking twice. Skipped along with the rest of peer
	// provisioning under --no-peer, since nothing is going to ssh there.
	if !*noPeer {
		target := setup.Target(config.Config{PeerHost: host, PeerUser: user})
		var stdin io.Reader
		if isTTY(stdout) {
			stdin = os.Stdin
		}
		if err := setup.EnsureAuth(target, stdin, stdout); err != nil {
			if setup.IsPeerUnreachable(err) {
				// Install already survives an unreachable peer; do not turn a
				// warning into a dead end here either.
				fmt.Fprintf(stderr, "could not reach %s (%v); continuing\n", host, err)
			} else {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
	}

	fmt.Fprintf(stdout, "choosing repos under %s\n", base)
	repos, err := chooseRepos(base, *all, *only, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if repos == nil {
		fmt.Fprintln(stdout, "cancelled; nothing was installed")
		return 0
	}

	if !*noPeer {
		fmt.Fprintf(stdout, "checking those repos on %s\n", host)
		remotes := cfgRemotes()
		if !checkPeer(config.Config{BaseDir: base, PeerHost: host, PeerUser: user, RemoteNames: remotes}, *peerBaseDir, repoWants(config.Config{BaseDir: base, RemoteNames: remotes}, repos), stdout, stderr) {
			fmt.Fprintln(stdout, "cancelled; nothing was installed and the peer was not touched")
			return 0
		}
	}

	fmt.Fprintln(stdout, "installing")
	if err := setup.Install(setup.Options{
		BaseDir: fs.Arg(0), PeerHost: host, PeerUser: user, Repos: repos,
		NoPeer: *noPeer, SelfHost: *selfHost, SelfUser: *selfUser,
		PeerBaseDir: *peerBaseDir, Out: stdout,
	}); err != nil {
		fmt.Fprintln(stderr, "install failed:", err)
		return 1
	}
	return 0
}

// chooseRepos resolves the allowlist: --all and --repos win outright, then the
// interactive picker, and failing both it is an error rather than a hang.
// A nil slice with a nil error means the user cancelled.
func chooseRepos(base string, all bool, only string, stdout, stderr io.Writer) ([]string, error) {
	discovered, err := scan.Repos(base, cfgRemotes())
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", base, err)
	}
	// A repo with no shared remote has nothing to sync through. Show it in the
	// picker (it is a real repo, and the fix is `git remote add`), but say so.

	if only != "" {
		return strings.Split(only, ","), nil
	}
	if all {
		out := make([]string, len(discovered))
		for i, r := range discovered {
			out[i] = r.Rel
		}
		return out, nil
	}

	// Pre-tick whatever is already being synced, so a re-run amends rather
	// than starts over.
	var current []string
	if cfg, cfgErr := config.Load(); cfgErr == nil {
		current = cfg.Repos
	}

	if !isTTY(stdout) {
		return nil, errors.New(
			"no terminal for the repo picker: pass --all or --repos a,b,c")
	}

	final, runErr := tea.NewProgram(picker.New(discovered, current)).Run()
	if runErr != nil {
		return nil, runErr
	}
	m, ok := final.(picker.Model)
	if !ok || m.Cancelled() {
		return nil, nil
	}
	return m.Selected(), nil
}

// isTTY reports whether w is an *os.File connected to a terminal.
func isTTY(w io.Writer) bool {
	f, isFile := w.(*os.File)
	return isFile && term.IsTerminal(int(f.Fd()))
}

// repoWants pairs each selected repo with the remote URL this machine syncs
// it through. A repo with no remote gets an empty URL, which the peer check
// then reports as a mismatch rather than pretending it is fine.
func repoWants(cfg config.Config, repos []string) []setup.RepoWant {
	out := make([]setup.RepoWant, 0, len(repos))
	for _, rel := range repos {
		w := setup.RepoWant{Rel: rel}
		dir := cfg.RepoPath(rel)
		if remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes()); err == nil {
			w.RemoteURL, _ = gitcmd.RemoteURL(dir, remote)
		}
		out = append(out, w)
	}
	return out
}

// checkPeer asks the peer which selected repos it has, prints the mismatches
// and returns whether to go ahead. The user can quit here with q, just as in
// the picker: nothing has been written yet, on either machine.
func checkPeer(cfg config.Config, peerBaseDir string, repos []setup.RepoWant, stdout, stderr io.Writer) bool {
	probe, err := setup.Probe(setup.Target(cfg))
	if err != nil {
		// Install already survives an unreachable peer; do not turn a warning
		// into a dead end here.
		fmt.Fprintf(stderr, "could not check %s (%v); continuing\n", cfg.PeerHost, err)
		return true
	}

	checks, err := setup.CheckPeerReposWithRemotes(setup.Target(cfg),
		setup.PeerBase(cfg.BaseDir, probe.Home, peerBaseDir), repos, cfg.Remotes())
	if err != nil {
		fmt.Fprintf(stderr, "could not check %s (%v); continuing\n", cfg.PeerHost, err)
		return true
	}
	n := setup.RenderRepoChecks(stdout, cfg.PeerHost,
		setup.PeerBase(cfg.BaseDir, probe.Home, peerBaseDir), checks)
	if n == 0 {
		return true
	}
	// Nothing to decide without a terminal: report and carry on, since the
	// mismatch is informational and the rest of the install is still correct.
	if !isTTY(stdout) {
		return true
	}
	return confirm(stdout, os.Stdin, "continue anyway? [enter] continue, [q] quit: ")
}

// confirm returns false only for an explicit quit. q, Q and EOF quit; anything
// else, including a bare enter, continues.
func confirm(w io.Writer, r io.Reader, question string) bool {
	fmt.Fprint(w, question)
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "q", "quit", "n", "no":
		return false
	}
	return true
}

// cfgRemotes returns the saved config's remote-name preference, or nil when
// there is no config yet, so a re-run honours a hand-edited remote_names.
func cfgRemotes() []string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	return cfg.Remotes()
}

// prompt writes label to out and reads a line from stdin, returning "" if
// stdin is not a terminal (so a scripted install fails fast instead of
// hanging).
func prompt(out io.Writer, label string) string {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return ""
	}
	fmt.Fprint(out, label)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return ""
	}
	return strings.TrimSpace(scanner.Text())
}

func cmdUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	purge := fs.Bool("purge", false, "also delete config and activity history")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := setup.Uninstall(*purge, stdout); err != nil {
		fmt.Fprintln(stderr, "uninstall failed:", err)
		return 1
	}
	return 0
}

func cmdReport(args []string, stdout, stderr io.Writer) int { return 1 }

func cmdHook(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "post-commit" {
		fmt.Fprintln(stderr, "usage: git-sync hook post-commit")
		return 2
	}
	wd, err := os.Getwd()
	if err != nil {
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		self = config.BinPath()
	}
	if err := syncer.Hook(wd, func(rel string) error {
		return syncer.SpawnDetached(self, rel)
	}); err != nil {
		// Never fail the commit over a sync problem.
		activity.AppendDebug("hook: " + err.Error())
	}
	return 0
}

func cmdPush(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: git-sync push <repo>")
		return 2
	}
	return syncer.Push(args[0])
}

func cmdReceive(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: git-sync receive <repo>")
		return 2
	}
	return syncer.Receive(args[0])
}

// cmdAskpass prints the stored password for the account baked into the shim.
// ssh execs this, reads one line of stdout, and uses it as the password.
func cmdAskpass(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return 2
	}
	// ssh passes the prompt text as an argument too; the account is args[0],
	// written into the shim at install time.
	pw, err := secret.Get(args[0])
	if err != nil {
		// Printing nothing makes ssh fail the auth rather than hang.
		fmt.Fprintln(stderr, "git-sync: no stored password for", args[0])
		return 1
	}
	fmt.Fprintln(stdout, string(pw))
	return 0
}

// cmdSavepass reads a password from stdin and stores it. Invoked over ssh on
// the peer, so the password crosses the encrypted channel and never appears
// in the remote command line or the peer's shell history.
func cmdSavepass(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: git-sync savepass <account>")
		return 2
	}
	pw, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil || len(bytes.TrimSpace(pw)) == 0 {
		fmt.Fprintln(stderr, "git-sync: empty password on stdin")
		return 1
	}
	if err := secret.Set(args[0], bytes.TrimRight(pw, "\r\n")); err != nil {
		fmt.Fprintln(stderr, "git-sync:", err)
		return 1
	}
	return 0
}
