package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/lock"
	"github.com/grillermo/git-sync/internal/picker"
	"github.com/grillermo/git-sync/internal/report"
	"github.com/grillermo/git-sync/internal/scan"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/syncer"
)

// peerFlag collects repeatable --peer user@host[:base_dir] values.
type peerFlag []config.Peer

func (f *peerFlag) String() string { return "" }

func (f *peerFlag) Set(s string) error {
	user, rest, ok := strings.Cut(s, "@")
	if !ok || user == "" || rest == "" {
		return fmt.Errorf("want user@host[:base_dir], got %q", s)
	}
	host, base, _ := strings.Cut(rest, ":")
	p := config.Peer{User: user, Host: host, BaseDir: base}
	if err := p.Validate(); err != nil {
		return err
	}
	*f = append(*f, p)
	return nil
}

func cmdInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var peerFlags peerFlag
	fs.Var(&peerFlags, "peer", "another machine in the mesh, as user@host[:base_dir] (repeatable)")
	peerHost := fs.String("peer-host", "", "hostname of the other machine (single-peer alias for --peer)")
	peerUser := fs.String("peer-user", "", "username on the other machine (single-peer alias for --peer)")
	all := fs.Bool("all", false, "sync every repo found; skip the picker")
	only := fs.String("repos", "", "comma-separated repos to sync; skips the picker")
	noPeer := fs.Bool("no-peer", false, "do not provision any peer machine")
	noInitialSync := fs.Bool("no-initial-sync", false,
		"do not push/fast-forward the selected repos level with their remotes")
	selfHost := fs.String("self-host", "", "this machine's hostname, as the peer sees it")
	selfUser := fs.String("self-user", "", "the account the peer should ssh back into")
	peerBaseDir := fs.String("peer-base-dir", "",
		"base_dir override for --peer-host/--peer-user (use user@host:base_dir with --peer instead)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: git-sync install [--peer user@host[:base_dir] ...] <base_dir>")
		return 2
	}

	// Step 1: --peer-host/--peer-user is the single-peer alias, folded into
	// the same config.Peer shape as a --peer value.
	var peers []config.Peer
	if *peerHost != "" || *peerUser != "" {
		p := config.Peer{Host: *peerHost, User: *peerUser, BaseDir: *peerBaseDir}
		if err := p.Validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		peers = append(peers, p)
	}
	peers = append(peers, peerFlags...)

	// Step 2: merge with whatever is already configured - a re-run adds to
	// the mesh rather than forgetting it - then prompt only if that leaves
	// nothing at all. config.Config.PeerList applies the same
	// validate/dedup/drop-self rules everything else in the mesh relies on,
	// and a flag-supplied peer wins over a stored one with the same target
	// since it is listed first.
	if cfg, err := config.Load(); err == nil {
		peers = append(peers, cfg.PeerList()...)
	}
	peers = config.Config{Peers: peers}.PeerList()

	if len(peers) == 0 && !*noPeer {
		host := prompt(stdout, "peer hostname: ")
		user := prompt(stdout, "peer username: ")
		if host == "" || user == "" {
			fmt.Fprintln(stderr, "at least one peer is required (--peer user@host, or --no-peer)")
			return 2
		}
		p := config.Peer{Host: host, User: user}
		if err := p.Validate(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		peers = append(peers, p)
	}

	base, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// Step 3: check every peer is reachable over key auth before picking
	// repos - asking after the user has already ticked forty repos would
	// waste that work if a peer turns out unreachable. Reported as a warning
	// either way; install continues regardless, same as every other
	// peer-unreachable case. Skipped along with the rest of peer provisioning
	// under --no-peer, since nothing is going to ssh anywhere.
	var reachable []config.Peer
	if !*noPeer {
		for _, p := range peers {
			if err := setup.Reachable(p.Target()); err != nil {
				fmt.Fprintf(stderr, "could not reach %s (%v); continuing\n", p.Host, err)
				continue
			}
			reachable = append(reachable, p)
		}

		// Step 4: every machine in the mesh has to be able to ssh to every
		// other - a missing key between two *peers*, a pair this machine
		// never exercises itself, would otherwise only show up later as a
		// failing notify nobody is watching. Warning only: the rest of the
		// mesh is still worth setting up.
		self := config.Peer{Host: resolveSelfHost(*selfHost), User: resolveSelfUser(*selfUser)}
		setup.RenderKeyChecks(stdout, setup.CheckKeys(self, peers))
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

	// Step 5: ask each reachable peer which of the chosen repos it actually
	// has, one machine at a time - the user can still quit here with q,
	// before anything is written on either machine.
	if !*noPeer {
		remotes := cfgRemotes()
		wants := repoWants(config.Config{BaseDir: base, RemoteNames: remotes}, repos)
		for _, p := range reachable {
			fmt.Fprintf(stdout, "checking those repos on %s\n", p.Host)
			if !checkPeer(p, config.Config{BaseDir: base, RemoteNames: remotes}, wants, stdout, stderr) {
				fmt.Fprintln(stdout, "cancelled; nothing was installed and the peers were not touched")
				return 0
			}
		}
	}

	fmt.Fprintln(stdout, "installing")
	if err := setup.Install(setup.Options{
		BaseDir: fs.Arg(0), Peers: peers, Repos: repos,
		NoPeer: *noPeer, SelfHost: *selfHost, SelfUser: *selfUser,
		PeerBaseDir: *peerBaseDir, Out: stdout,
	}); err != nil {
		fmt.Fprintln(stderr, "install failed:", err)
		return 1
	}

	// Step 7 / stage "level": bring every machine up to the shared remote now
	// that the hook is armed everywhere. A repo that was already out of step
	// stays out of step forever otherwise - receive only ever fast-forwards,
	// so a single unpushed commit on any machine makes every later sync warn
	// instead of applying, and nothing retries it.
	if !*noPeer && !*noInitialSync {
		levelRepos(config.Config{BaseDir: base, RemoteNames: cfgRemotes()}, peers, repos, stdout, stderr)
	}
	return 0
}

// resolveSelfHost is the hostname install tells a peer to reach this machine
// back on, mirroring setup.Install's own default so the key check asks about
// exactly the same identity install will provision.
func resolveSelfHost(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	h, _ := os.Hostname()
	return h
}

// resolveSelfUser mirrors resolveSelfHost for the account the peer ssh's
// back into.
func resolveSelfUser(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("USER")
}

// levelRepos runs the initial synchronisation: measure every machine in the
// mesh against the shared remote, say what it will do, then push and
// fast-forward. One peer failing to answer never stops the others from being
// levelled - it is reported and skipped.
//
// Never fatal. The install itself has already succeeded by this point, and a
// repo that cannot be levelled is a thing the user must fix by hand in the
// repo - not a reason to leave the machine unconfigured.
func levelRepos(cfg config.Config, peers []config.Peer, repos []string, stdout, stderr io.Writer) {
	fmt.Fprintln(stdout, "levelling repos with their remotes")

	var targets []setup.PeerTarget
	for _, p := range peers {
		probe, err := setup.Probe(p.Target())
		if err != nil {
			fmt.Fprintf(stderr, "could not reach %s (%v); skipping the initial sync there\n", p.Host, err)
			continue
		}
		targets = append(targets, setup.PeerTarget{
			Peer: p, BaseDir: setup.PeerBase(cfg.BaseDir, probe.Home, p.BaseDir),
		})
	}
	if len(targets) == 0 {
		fmt.Fprintln(stderr, "no reachable peers; skipping the initial sync")
		return
	}

	measured, err := setup.MeasureSync(cfg, targets, repos)
	if err != nil {
		fmt.Fprintf(stderr, "could not fully compare with the mesh (%v); continuing with what could be measured\n", err)
	}
	if !setup.RenderSyncPlan(stdout, measured) {
		return
	}

	// This is the one point where git-sync pushes commits the user did not
	// just make, so on a terminal it asks first.
	if isTTY(stdout) && !confirm(stdout,
		os.Stdin, "push and fast-forward these now? [enter] yes, [q] skip: ") {
		fmt.Fprintln(stdout, "skipped; run install again to level them later")
		return
	}
	setup.RenderSyncResult(stdout, setup.ApplySync(cfg, targets, measured))
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

// checkPeer asks one peer which selected repos it has, prints the mismatches
// and returns whether to go ahead. The user can quit here with q, just as in
// the picker: nothing has been written yet, on any machine. Called once per
// reachable peer, so a mismatch on one machine never hides one on another.
func checkPeer(peer config.Peer, cfg config.Config, repos []setup.RepoWant, stdout, stderr io.Writer) bool {
	target := peer.Target()
	probe, err := setup.Probe(target)
	if err != nil {
		// Install already survives an unreachable peer; do not turn a warning
		// into a dead end here.
		fmt.Fprintf(stderr, "could not check %s (%v); continuing\n", peer.Host, err)
		return true
	}

	peerBase := setup.PeerBase(cfg.BaseDir, probe.Home, peer.BaseDir)
	checks, err := setup.CheckPeerReposWithRemotes(target, peerBase, repos, cfg.Remotes())
	if err != nil {
		fmt.Fprintf(stderr, "could not check %s (%v); continuing\n", peer.Host, err)
		return true
	}
	n := setup.RenderRepoChecks(stdout, peer.Host, peerBase, checks)
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
	local := fs.Bool("local", false, "only uninstall this machine, not the rest of the mesh")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *local {
		if err := setup.Uninstall(*purge, stdout); err != nil {
			fmt.Fprintln(stderr, "uninstall failed:", err)
			return 1
		}
		return 0
	}

	// No config (never installed, or already uninstalled) means there is no
	// mesh to know about - fall back to the same local-only uninstall.
	cfg, err := config.Load()
	if err != nil {
		if err := setup.Uninstall(*purge, stdout); err != nil {
			fmt.Fprintln(stderr, "uninstall failed:", err)
			return 1
		}
		return 0
	}

	if err := setup.UninstallMesh(cfg, *purge, stdout); err != nil {
		fmt.Fprintln(stderr, "uninstall failed:", err)
		return 1
	}
	return 0
}

func cmdReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.Duration("since", 0, "only show activity newer than this (e.g. 24h)")
	repo := fs.String("repo", "", "only show repos whose path contains this")
	problems := fs.Bool("errors", false, "only show warnings and errors")
	plain := fs.Bool("plain", false, "force static output even on a terminal")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	events, err := activity.Read()
	if err != nil {
		fmt.Fprintln(stderr, "reading activity log:", err)
		return 1
	}
	opts := report.Options{Repo: *repo, ProblemsOnly: *problems}
	if *since > 0 {
		opts.Since = time.Now().Add(-*since)
	}
	summaries := report.Summarize(report.Filter(events, opts))

	// Static output when piped, so the report stays greppable and scriptable.
	interactive := !*plain && isTTY(stdout)
	if !interactive {
		report.WritePlain(stdout, summaries)
		return 0
	}

	if _, err := tea.NewProgram(report.NewModel(summaries), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(stderr, "report:", err)
		return 1
	}
	return 0
}

func cmdHook(args []string, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: git-sync hook <post-commit|pre-commit|pre-push>")
		return 2
	}
	wd, err := os.Getwd()
	if err != nil {
		return 0 // never block a commit over our own failure
	}
	switch args[0] {
	case "pre-commit", "pre-push":
		return syncer.Block(wd, stderr)
	case "post-commit":
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
	default:
		fmt.Fprintln(stderr, "usage: git-sync hook <post-commit|pre-commit|pre-push>")
		return 2
	}
}

// cmdUnlock clears a receiver lock left behind by a receive that died.
func cmdUnlock(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "unlock:", err)
		return 1
	}
	rel := ""
	if len(args) > 0 {
		rel = args[0]
	} else {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "unlock:", err)
			return 1
		}
		root, err := gitcmd.Toplevel(wd)
		if err != nil {
			fmt.Fprintln(stderr, "unlock: not inside a git repo; name one: git-sync unlock <repo>")
			return 2
		}
		// git resolves symlinks in --show-toplevel (e.g. macOS's /var ->
		// /private/var), but base_dir as configured is not resolved.
		// Resolve here too, or a repo under a symlinked ancestor looks
		// "outside base_dir". Mirrors syncer's repoRel.
		rc := cfg
		if resolved, err := filepath.EvalSymlinks(rc.BaseDir); err == nil {
			rc.BaseDir = resolved
		}
		if rel, err = rc.RepoRel(root); err != nil {
			fmt.Fprintln(stderr, "unlock:", err)
			return 2
		}
	}
	if err := cfg.ValidateRel(rel); err != nil {
		fmt.Fprintln(stderr, "unlock:", err)
		return 2
	}
	owner, had, err := lock.Break(rel)
	if err != nil {
		fmt.Fprintln(stderr, "unlock:", err)
		return 1
	}
	if !had {
		fmt.Fprintf(stdout, "%s is not locked\n", rel)
		return 0
	}
	from := owner.From
	if from == "" {
		from = "an unknown machine"
	}
	fmt.Fprintf(stdout, "cleared the lock on %s (held by %s since %s)\n",
		rel, from, owner.Started.Format(time.RFC3339))
	_ = activity.Append(activity.Event{
		Repo: rel, Op: activity.OpReceive, Status: activity.StatusWarn,
		Peer: owner.From, Msg: "lock cleared by hand",
	})
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
	// `receive <repo> --from <host>`: flag.Parse stops at <repo>, so pull a
	// leading non-flag argument out before parsing.
	var rest []string
	repo := ""
	for _, a := range args {
		if repo == "" && !strings.HasPrefix(a, "-") {
			repo = a
			continue
		}
		rest = append(rest, a)
	}

	fs := flag.NewFlagSet("receive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "the machine that sent this notification")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if repo == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: git-sync receive <repo> [--from <host>]")
		return 2
	}
	return syncer.Receive(repo, *from)
}
