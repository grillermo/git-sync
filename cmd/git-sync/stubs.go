package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/discovery"
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
	noService := fs.Bool("no-service", false,
		"do not install the login service that announces each machine to the mesh when it starts")
	discover := fs.Bool("discover", false,
		"look for more machines on this network even when peers are already configured")
	noInitialSync := fs.Bool("no-initial-sync", false,
		"do not push/fast-forward the selected repos level with their remotes")
	noClone := fs.Bool("no-clone", false,
		"do not clone a selected repo onto a machine that does not have it")
	selfHost := fs.String("self-host", "", "this machine's hostname, as the peer sees it")
	selfUser := fs.String("self-user", "", "the account the peer should ssh back into")
	peerBaseDir := fs.String("peer-base-dir", "",
		"base_dir override for --peer-host/--peer-user (use user@host:base_dir with --peer instead)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// With no base_dir, install syncs just the repo it is run in: no scan, no
	// repo picker, but always the machine picker, since choosing where this
	// repo goes is the point.
	here := fs.NArg() == 0
	if fs.NArg() > 1 || (here && (*all || *only != "")) {
		fmt.Fprintln(stderr, "usage: git-sync install [--peer user@host[:base_dir] ...] [<base_dir>]")
		return 2
	}
	var baseArg, hereRel string
	if here {
		var err error
		if baseArg, hereRel, err = installHere(stdout); err != nil {
			fmt.Fprintln(stderr, "install:", err)
			return 2
		}
		*discover = true
	} else {
		baseArg = fs.Arg(0)
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

	// Step 2a: on a terminal, offer the machines on this network instead of
	// asking for a hostname - always with --discover, otherwise only when
	// there is no peer yet. Ticking nothing falls through to the prompt below;
	// a discovery that cannot run at all is only a warning, for the same
	// reason.
	if !*noPeer && (*discover || len(peers) == 0) && isTTY(stdout) {
		fmt.Fprintf(stdout, "looking for machines on this network (%s)\n", discovery.ScanDuration)
		found, ok, err := discoverPeers(peers, stdout)
		switch {
		case err != nil:
			fmt.Fprintln(stderr, "could not look for machines:", err)
		case !ok:
			fmt.Fprintln(stdout, "cancelled; nothing was installed")
			return 0
		}
		peers = config.Config{Peers: append(peers, found...)}.PeerList()
	}

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

	base, err := filepath.Abs(baseArg)
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
	paired := false
	if !*noPeer {
		self := config.Peer{Host: resolveSelfHost(*selfHost), User: resolveSelfUser(*selfUser)}
		reachable = connect(self, peers, stdout, stderr)

		// Step 4: every machine in the mesh has to be able to ssh to every
		// other - a missing key between two *peers*, a pair this machine
		// never exercises itself, would otherwise only show up later as a
		// failing notify nobody is watching. Warning only: the rest of the
		// mesh is still worth setting up.
		fixes := setup.RenderKeyChecks(stdout, self, setup.CheckKeys(self, peers))
		if offerFixes(fixes, stdout, stderr) {
			fixes = setup.RenderKeyChecks(stdout, self, setup.CheckKeys(self, peers))
			if len(fixes) == 0 {
				fmt.Fprintln(stdout, "every pair of machines connects now")
			}
		}
		paired = len(reachable) == len(peers) && len(fixes) == 0
	}

	// repos is the whole allowlist install writes; added is the part of it
	// this run is about, which is all the peer check, the clones and the
	// levelling look at.
	var repos, added []string
	if here {
		added = []string{hereRel}
		repos = withRepo(hereRel)
		fmt.Fprintf(stdout, "syncing %s (base_dir %s)\n", hereRel, base)
	} else {
		fmt.Fprintf(stdout, "choosing repos under %s\n", base)
		repos, err = chooseRepos(base, *all, *only, stdout, stderr)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if repos == nil {
			fmt.Fprintln(stdout, "cancelled; nothing was installed")
			return 0
		}
		added = repos
	}

	// Step 5: ask each reachable peer which of the chosen repos it actually
	// has, one machine at a time - the user can still quit here with q,
	// before anything is written on either machine. A repo a peer is missing
	// is cloned there once the install itself has succeeded (step 6a).
	var clones []peerClones
	if !*noPeer {
		remotes := cfgRemotes()
		wants := repoWants(config.Config{BaseDir: base, RemoteNames: remotes}, added)
		var ok bool
		clones, ok = checkPeer(reachable, config.Config{BaseDir: base, RemoteNames: remotes}, wants,
			!*noClone, stdout, stderr)
		if !ok {
			fmt.Fprintln(stdout, "cancelled; nothing was installed and the peers were not touched")
			return 0
		}
	}

	// Step 6: this machine's own identity is what every peer will use to
	// notify us back - config.Peer{Host: selfHost, User: selfUser, BaseDir:
	// cfg.BaseDir} gets marshalled into every peer's config.toml, and
	// Peer.Validate (run inside peerListFor/MarshalFor) silently drops it if
	// it fails - an unset $USER, or a --self-host/--self-user/base_dir
	// with a shell-unsafe character. That would leave every peer unable to
	// ever notify this machine again, with no error anywhere. Validate it
	// here, before anything is written to a peer, so a bad identity fails
	// the install instead of failing silently forever.
	if !*noPeer {
		self := config.Peer{Host: resolveSelfHost(*selfHost), User: resolveSelfUser(*selfUser), BaseDir: base}
		if err := self.Validate(); err != nil {
			fmt.Fprintln(stderr, "this machine's own identity is invalid for mesh notifications:", err)
			fmt.Fprintln(stderr, "every peer would be provisioned with a self entry it silently drops, "+
				"leaving them unable to ever notify this machine back; fix --self-host/--self-user "+
				"(or $USER) and re-run install")
			return 1
		}
	}

	fmt.Fprintln(stdout, "installing")
	if err := setup.Install(setup.Options{
		BaseDir: baseArg, Peers: peers, Repos: repos,
		NoPeer: *noPeer, SelfHost: *selfHost, SelfUser: *selfUser,
		PeerBaseDir: *peerBaseDir, NoService: *noService, Out: stdout,
	}); err != nil {
		fmt.Fprintln(stderr, "install failed:", err)
		return 1
	}

	// The saved repo selection has done its job once every machine is
	// reachable and every pair connects; until then the next run reuses it.
	// A single-repo install never used the picker, so has nothing to keep.
	switch {
	case here:
	case paired || *noPeer:
		clearPending()
	case loadPending(base) != nil:
		fmt.Fprintf(stdout, "not every machine is paired yet; your repo selection is kept in %s "+
			"for the next run\n", pendingPath())
	}

	// Step 6a: clone every selected repo onto each machine that lacks it, so
	// a repo that exists on only one machine is synced from the start rather
	// than skipped on every commit. Before levelling, so the new clones are
	// measured along with everything else.
	cloneMissing(clones, stdout)

	// Step 7 / stage "level": bring every machine up to the shared remote now
	// that the hook is armed everywhere. A repo that was already out of step
	// stays out of step forever otherwise - receive only ever fast-forwards,
	// so a single unpushed commit on any machine makes every later sync warn
	// instead of applying, and nothing retries it.
	if !*noPeer && !*noInitialSync {
		levelRepos(config.Config{BaseDir: base, RemoteNames: cfgRemotes()}, peers, added, stdout, stderr)
	}
	return 0
}

// installHere resolves `git-sync install` run with no base_dir to the repo the
// current directory is in. An existing install keeps its base_dir, and the
// repo has to be under it - moving base_dir would change the identity of
// every repo already syncing. With nothing installed yet the user is asked
// for one, defaulting to the repo's parent.
func installHere(stdout io.Writer) (base, rel string, err error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	root, err := gitcmd.Toplevel(wd)
	if err != nil {
		return "", "", errors.New("not inside a git repo; run it in one, or name a base_dir to pick repos under")
	}

	if cfg, cfgErr := config.Load(); cfgErr == nil && cfg.BaseDir != "" {
		base = cfg.BaseDir
	} else {
		def := filepath.Dir(root)
		base = prompt(stdout, fmt.Sprintf("base_dir, the folder every synced repo lives under [%s]: ", def))
		if base == "" {
			base = def
		} else if rest, ok := strings.CutPrefix(base, "~/"); ok {
			base = filepath.Join(os.Getenv("HOME"), rest)
		}
		if base, err = filepath.Abs(base); err != nil {
			return "", "", err
		}
	}

	// git resolves symlinks in --show-toplevel (e.g. macOS's /var ->
	// /private/var) but base_dir is not, so compare them resolved, as unlock
	// does.
	rc := config.Config{BaseDir: base}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		rc.BaseDir = resolved
	}
	if rel, err = rc.RepoRel(root); err != nil {
		return "", "", err
	}
	return base, rel, nil
}

// withRepo is the configured allowlist with rel added, so syncing one more
// repo never drops the ones already syncing.
func withRepo(rel string) []string {
	var repos []string
	if cfg, err := config.Load(); err == nil {
		repos = cfg.Repos
	}
	if (config.Config{Repos: repos}).IsSelected(rel) {
		return repos
	}
	return append(repos, rel)
}

// discoverPeers scans the local network behind the machine picker and turns
// the hosts ticked into peers, asking once for the account to use on them -
// the same one as here, unless the user says otherwise. ok is false when the
// user cancelled the picker.
func discoverPeers(current []config.Peer, stdout io.Writer) ([]config.Peer, bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hosts, ok, err := picker.ChoosePeers(discovery.Scan(ctx), current)
	if err != nil || !ok || len(hosts) == 0 {
		return nil, ok, err
	}
	def := resolveSelfUser("")
	user := prompt(stdout, fmt.Sprintf("username on those machines [%s]: ", def))
	if user == "" {
		user = def
	}
	return peersFrom(hosts, user, stdout)
}

// peersFrom pairs each discovered host with user. A hostname is untrusted
// input from the network and ends up inside remote shell commands, so one
// that would not be a valid peer is skipped with a warning; a bad user is the
// user's own typo and fails the lot.
func peersFrom(hosts []string, user string, w io.Writer) ([]config.Peer, bool, error) {
	if err := (config.Peer{Host: "x", User: user}).Validate(); err != nil {
		return nil, false, err
	}
	out := make([]config.Peer, 0, len(hosts))
	for _, h := range hosts {
		p := config.Peer{Host: h, User: user}
		if err := p.Validate(); err != nil {
			fmt.Fprintf(w, "skipping %q: %v\n", h, err)
			continue
		}
		out = append(out, p)
	}
	return out, true, nil
}

// connect checks this machine can reach every peer over key-only ssh, says
// exactly what is wrong with each one that it cannot, offers to run the fixes,
// and returns the peers that answer.
func connect(self config.Peer, peers []config.Peer, stdout, stderr io.Writer) []config.Peer {
	reach := func() ([]config.Peer, []setup.Fix) {
		var ok []config.Peer
		var results []setup.KeyResult
		for _, p := range peers {
			r := setup.Reach(self, p)
			if r.OK {
				ok = append(ok, p)
			}
			results = append(results, r)
		}
		return ok, setup.FixesFor(self, results)
	}
	render := func(fixes []setup.Fix) {
		fmt.Fprintf(stderr, "could not reach %d of %d machines:\n", len(fixes), len(peers))
		for _, f := range fixes {
			setup.RenderFix(stderr, f)
		}
	}

	ok, fixes := reach()
	if len(fixes) == 0 {
		return ok
	}
	render(fixes)
	if offerFixes(fixes, stdout, stderr) {
		if ok, fixes = reach(); len(fixes) > 0 {
			render(fixes)
		}
	}
	if len(fixes) > 0 {
		fmt.Fprintln(stderr, "continuing without them; run install again once they are fixed")
	}
	return ok
}

// offerFixes asks, on a terminal, to run every fix that has commands, and
// reports whether it ran any. A fix that throws away a recorded host key is
// asked about on its own and needs a typed "yes": that is the one case where
// the "fix" could be letting an impostor in.
func offerFixes(fixes []setup.Fix, stdout, stderr io.Writer) bool {
	var safe, risky []setup.Fix
	for _, f := range fixes {
		switch {
		case len(f.Steps) == 0:
		case f.Risky:
			risky = append(risky, f)
		default:
			safe = append(safe, f)
		}
	}
	if len(safe)+len(risky) == 0 || !isTTY(stdout) {
		return false
	}

	var run []setup.Fix
	if len(safe) > 0 && confirm(stdout, os.Stdin,
		fmt.Sprintf("run the %d fix command(s) above now? they are interactive [enter] yes, [q] skip: ", len(safe))) {
		run = append(run, safe...)
	}
	for _, f := range risky {
		fmt.Fprintf(stdout, "%s -> %s: its host key CHANGED. Only replace it if you know why "+
			"(reinstalled, new hardware, reused address).\n", f.From.Host, f.To.Host)
		if confirmYes(stdout, os.Stdin, "type yes to forget the old key and trust the new one: ") {
			run = append(run, f)
		}
	}
	for _, f := range run {
		fmt.Fprintf(stdout, "fixing %s -> %s\n", f.From.Host, f.To.Host)
		if err := setup.RunFix(f, func(argv []string) error {
			fmt.Fprintf(stdout, "$ %s\n", strings.Join(argv, " "))
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		}); err != nil {
			fmt.Fprintf(stderr, "fix failed: %v\n", err)
		}
	}
	return len(run) > 0
}

// confirmYes is confirm for the one question where a bare enter must not
// mean yes.
func confirmYes(w io.Writer, r io.Reader, question string) bool {
	fmt.Fprint(w, question)
	sc := bufio.NewScanner(r)
	return sc.Scan() && strings.EqualFold(strings.TrimSpace(sc.Text()), "yes")
}

// resolveSelfHost is the address install tells a peer to reach this machine
// back on, mirroring setup.Install's own default so the key check asks about
// exactly the same identity install will provision.
func resolveSelfHost(flagVal string) string {
	return setup.SelfHost(flagVal)
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
//
// A selection made in the picker is saved (see pendingRepos) until a run
// pairs every machine; in between, the picker opens with it pre-ticked, and
// without a terminal it is reused as is.
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

	pending := loadPending(base)

	// Whatever is already being synced stays ticked, so a re-run amends
	// rather than starts over; whatever an unfinished run picked is ticked
	// too, but among the new repos - it is not syncing yet.
	var current []string
	if cfg, cfgErr := config.Load(); cfgErr == nil {
		current = cfg.Repos
	}

	if !isTTY(stdout) {
		if pending != nil {
			return pending, nil
		}
		return nil, errors.New(
			"no terminal for the repo picker: pass --all or --repos a,b,c")
	}

	repos, ok, err := picker.Choose(discovered, current, pending)
	if err != nil || !ok {
		return nil, err
	}
	if err := savePending(base, repos); err != nil {
		fmt.Fprintln(stderr, "could not save the repo selection for the next run:", err)
	}
	return repos, nil
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
			if w.RemoteURL, err = gitcmd.RemoteURL(dir, remote); err == nil {
				w.Remote = remote
			}
		}
		w.Branch, _ = gitcmd.CurrentBranch(dir)
		out = append(out, w)
	}
	return out
}

// peerClones is the repos one peer is missing that install will clone there.
type peerClones struct {
	Target setup.PeerTarget
	Repos  []setup.RepoWant
}

// checkPeer asks every reachable peer which selected repos it has, prints
// the mismatches per machine, and returns whether to go ahead. The user can
// quit here with q, just as in the picker: nothing has been written yet, on
// any machine. Called once for the whole mesh so that a mismatch on one
// machine is reported alongside the others rather than behind its own
// separate confirm prompt. With clone set, it also returns the missing repos
// to clone on each peer; those are announced but do not need confirming.
func checkPeer(peers []config.Peer, cfg config.Config, repos []setup.RepoWant, clone bool,
	stdout, stderr io.Writer) ([]peerClones, bool) {
	var targets []setup.PeerTarget
	for _, p := range peers {
		fmt.Fprintf(stdout, "checking those repos on %s\n", p.Host)
		probe, err := setup.Probe(p.Target())
		if err != nil {
			// Install already survives an unreachable peer; do not turn a
			// warning into a dead end here.
			fmt.Fprintf(stderr, "could not check %s (%v); continuing\n", p.Host, err)
			continue
		}
		peerBase := setup.PeerBase(cfg.BaseDir, probe.Home, p.BaseDir)
		targets = append(targets, setup.PeerTarget{Peer: p, BaseDir: peerBase})
	}
	if len(targets) == 0 {
		return nil, true
	}

	results := setup.CheckPeers(targets, repos, cfg.Remotes())
	total := 0
	var clones []peerClones
	for _, pt := range targets {
		checks := results[pt.Peer.Host]
		total += setup.RenderRepoChecks(stdout, pt.Peer.Host, pt.BaseDir, checks, clone)
		if !clone {
			continue
		}
		pc := peerClones{Target: pt}
		for _, c := range checks {
			if c.WillClone() {
				pc.Repos = append(pc.Repos, c.Want)
			}
		}
		if len(pc.Repos) > 0 {
			clones = append(clones, pc)
		}
	}
	if total == 0 {
		return clones, true
	}
	// Nothing to decide without a terminal: report and carry on, since the
	// mismatch is informational and the rest of the install is still correct.
	if !isTTY(stdout) {
		return clones, true
	}
	return clones, confirm(stdout, os.Stdin, "continue anyway? [enter] continue, [q] quit: ")
}

// cloneMissing clones, on each peer, the selected repos checkPeer found it
// missing. Never fatal: a clone that fails (the peer has no credentials for
// the remote, say) is reported with what to do, and the rest of the mesh is
// still installed.
func cloneMissing(clones []peerClones, stdout io.Writer) {
	for _, pc := range clones {
		fmt.Fprintf(stdout, "cloning %d missing repo(s) on %s\n", len(pc.Repos), pc.Target.Peer.Host)
		// An unreachable peer already marks every result, so the error adds
		// nothing the rendered results do not say.
		results, _ := setup.ClonePeerRepos(pc.Target.Peer.Target(), pc.Target.BaseDir, pc.Repos)
		setup.RenderCloneResults(stdout, pc.Target.Peer.Host, results)
	}
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

var errNotInRepo = errors.New("not inside a git repo; name one")

// currentRel is the selected-repo relpath of the repo containing the
// working directory, for commands whose <repo> argument is optional.
func currentRel(cfg config.Config) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := gitcmd.Toplevel(wd)
	if err != nil {
		return "", errNotInRepo
	}
	// git resolves symlinks in --show-toplevel (e.g. macOS's /var ->
	// /private/var), but base_dir as configured is not resolved.
	// Resolve here too, or a repo under a symlinked ancestor looks
	// "outside base_dir". Mirrors syncer's repoRel.
	rc := cfg
	if resolved, err := filepath.EvalSymlinks(rc.BaseDir); err == nil {
		rc.BaseDir = resolved
	}
	return rc.RepoRel(root)
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
		if rel, err = currentRel(cfg); err != nil {
			if errors.Is(err, errNotInRepo) {
				fmt.Fprintln(stderr, "unlock: not inside a git repo; name one: git-sync unlock <repo>")
			} else {
				fmt.Fprintln(stderr, "unlock:", err)
			}
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
	code := syncer.Receive(repo, *from)
	kickActivate()

	// Being notified proves this machine is back online, so anything it
	// failed to deliver while it was away can go now - in the background, so
	// the notifying peer's ssh is not held open by it.
	if syncer.HasPending() {
		if self, err := os.Executable(); err == nil {
			_ = syncer.SpawnRetry(self)
		}
	}
	return code
}

// kickActivate starts the activate drainer if anything is queued. Called
// only once the receive lock is released, so the drainer can take it.
func kickActivate() {
	if !syncer.HasActivateQueue() {
		return
	}
	if self, err := os.Executable(); err == nil {
		_ = syncer.SpawnActivateDrain(self)
	}
}

// cmdActivate is `git-sync activate [<repo>]` for humans, and
// `git-sync activate --drain` for the background drainer.
func cmdActivate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--drain" {
		return syncer.DrainActivate()
	}
	if len(args) > 1 {
		fmt.Fprintln(stderr, "usage: git-sync activate [<repo>]")
		return 2
	}
	rel := ""
	if len(args) == 1 {
		rel = args[0]
	} else {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(stderr, "activate:", err)
			return 1
		}
		if rel, err = currentRel(cfg); err != nil {
			fmt.Fprintln(stderr, "activate:", err)
			return 2
		}
	}

	// Ctrl-C or SIGTERM cancels the context, which kills the script's
	// process group and lets ActivateNow release its locks (a leaked repo
	// lock would refuse commits for minutes).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() { // a second Ctrl-C then kills us outright
		<-ctx.Done()
		stop()
	}()
	code := syncer.ActivateNowContext(ctx, rel, stdout)
	// Entries queued while this run held the drain lock found their drainer
	// exiting on the busy lock; start one now the locks are released.
	if ctx.Err() == nil {
		kickActivate()
	}
	return code
}

func cmdRetry(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: git-sync retry")
		return 2
	}
	return syncer.Retry()
}

func cmdAnnounce(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: git-sync announce")
		return 2
	}
	code := syncer.Announce()
	kickActivate()
	return code
}

// cmdWatch is what the login service runs. launchd and systemd stop it with
// SIGTERM; that ends it at the next safe point rather than mid-receive.
func cmdWatch(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: git-sync watch")
		return 2
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	stop := make(chan struct{})
	go func() {
		<-sigs
		close(stop)
	}()
	return syncer.Watch(syncer.WatchOptions{
		BinPath: config.BinPath(),
		StillWanted: func() bool {
			_, ok := setup.ServiceInstalled()
			return ok
		},
		Stop:          stop,
		AfterAnnounce: kickActivate,
	})
}

func cmdService(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: git-sync service install|uninstall|status [--local]"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("service "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	local := fs.Bool("local", false, "only this machine, not the rest of the mesh")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	switch action {
	case "status":
		if path, ok := setup.ServiceInstalled(); ok {
			fmt.Fprintf(stdout, "login service installed: %s\n", path)
		} else {
			fmt.Fprintln(stdout, "login service not installed (git-sync service install)")
		}
		return 0
	case "install", "uninstall":
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
	remove := action == "uninstall"

	if *local {
		var err error
		if remove {
			err = setup.UninstallService(stdout)
		} else {
			err = setup.InstallService(stdout)
		}
		if err != nil {
			fmt.Fprintf(stderr, "service %s failed: %v\n", action, err)
			return 1
		}
		return 0
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "git-sync is not installed here; run git-sync install first")
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "locating this binary:", err)
		return 1
	}
	if err := setup.ServiceMesh(cfg, self, remove, stdout); err != nil {
		fmt.Fprintf(stderr, "service %s failed: %v\n", action, err)
		return 1
	}
	return 0
}
