package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
)

// Initial synchronisation: get every machine in the mesh level with the
// shared remote *before* the hook is armed, so the first commit after
// install is an ordinary fast-forward everywhere else.
//
// Without this step, a repo that was already out of step at install time
// stays broken silently. The failure looks exactly like the one this
// package exists to prevent: the pusher reports "pushed" and "peer synced",
// while the peer's receive refuses the fast-forward and warns into a log
// nobody is watching. A machine holding one unpushed commit from last week
// is enough to cause it, and it never resolves on its own, because receive
// never pushes.
//
// The repair uses only the two operations the steady state already relies on -
// push, and merge --ff-only - so it can add nothing to history that a normal
// sync would not. Whatever those two cannot fix is genuinely diverged, and
// merging is the user's call, not ours.

// SyncPos is where one machine's copy of a repo sits relative to the shared
// remote. Err is set when we could not measure it at all, in which case the
// counts are meaningless.
type SyncPos struct {
	Branch string
	Remote string
	Ahead  int // commits here that the remote does not have
	Behind int // commits on the remote that this machine does not have
	Err    string
	Note   string // something the user must know even though the sync worked
}

func (p SyncPos) ok() bool        { return p.Err == "" }
func (p SyncPos) diverged() bool  { return p.ok() && p.Ahead > 0 && p.Behind > 0 }
func (p SyncPos) canPush() bool   { return p.ok() && p.Ahead > 0 && p.Behind == 0 }
func (p SyncPos) canFF() bool     { return p.ok() && p.Behind > 0 && p.Ahead == 0 }
func (p SyncPos) converged() bool { return p.ok() && p.Ahead == 0 && p.Behind == 0 }

// PeerTarget is one machine to measure, with its own sync root. BaseDir is
// that peer's base_dir as seen on the peer itself (the answer PeerBase or a
// config's own base_dir gives), never this machine's.
type PeerTarget struct {
	Peer    config.Peer
	BaseDir string
}

// PeerPos is one peer machine's position for one repo.
type PeerPos struct {
	Peer config.Peer
	Pos  SyncPos
}

// RepoSync is one selected repo measured on every machine.
type RepoSync struct {
	Rel   string
	Here  SyncPos
	There []PeerPos
}

// needsWork reports whether this repo is anything other than fully healthy -
// either it can be repaired, or it needs the user. Blocked counts even when
// every machine is individually converged: two machines sitting level on
// *different branches* each look fine alone, and the pair still never syncs.
func (r RepoSync) needsWork() bool {
	if r.blocked() || !r.Here.converged() {
		return true
	}
	for _, p := range r.There {
		if !p.Pos.converged() {
			return true
		}
	}
	return false
}

// blocked reports whether push and fast-forward cannot fix this repo, so the
// user has to. Two machines ahead is not blocked: the first pushes, and the
// others are reported afterwards by the re-measure.
func (r RepoSync) blocked() bool {
	if !r.Here.ok() {
		return true
	}
	for _, p := range r.There {
		if !p.Pos.ok() || p.Pos.Branch != r.Here.Branch {
			return true
		}
	}
	return false
}

// publisher identifies which machine ApplySync would push from for r,
// mirroring its own selection order: this machine first, then the first peer
// - in peer-list order - that is purely ahead. It exists only to describe the
// plan; ApplySync makes the real choice independently and this must always
// agree with it.
func (r RepoSync) publisher() (here bool, peerHost string, ok bool) {
	if r.blocked() {
		return false, "", false
	}
	if r.Here.canPush() {
		return true, "", true
	}
	for _, p := range r.There {
		if p.Pos.canPush() {
			return false, p.Peer.Host, true
		}
	}
	return false, "", false
}

// initialSyncMarker opens the remote scripts, so they are identifiable in the
// debug log and in the ssh stub's recorded calls.
const initialSyncMarker = "# git-sync-initial-sync"

// MeasureSync reports, for each repo, how far each machine is from the shared
// remote. It fetches here and on every peer first: the question is about the
// remote as it is now, not as it was at the last fetch.
//
// Measuring is read-only, so it is safe to run before asking the user
// anything. RenderSyncPlan turns the result into a description, ApplySync
// acts on it.
//
// Peers are asked sequentially - this is install, not the hot path - and one
// peer failing to answer never stops the others from being measured: the
// returned slice always has one row per repo with one PeerPos per peer,
// whatever the error. The returned error, when non-nil, wraps
// errPeerUnreachable for every peer that could not be reached at all, purely
// so a caller can tell the user; ApplySync's own re-measure deliberately
// ignores it and trusts the per-repo/per-peer data instead.
func MeasureSync(cfg config.Config, peers []PeerTarget, repos []string) ([]RepoSync, error) {
	out := make([]RepoSync, 0, len(repos))
	askable := make([]string, 0, len(repos))
	badRel := map[string]bool{}
	for _, rel := range repos {
		rs := RepoSync{Rel: rel, Here: measureHere(cfg, rel)}
		if strings.Contains(rel, "'") {
			// Cannot be named safely in the remote shell command, and a repo we
			// cannot ask about must not be reported as fine.
			badRel[rel] = true
		} else {
			askable = append(askable, rel)
		}
		out = append(out, rs)
	}

	for _, name := range cfg.Remotes() {
		if strings.Contains(name, "'") {
			return out, fmt.Errorf("remote name %q is not usable over ssh", name)
		}
	}

	var errs []error
	for _, pt := range peers {
		positions := map[string]SyncPos{}
		var sshErr error
		switch {
		case len(askable) == 0:
			// Nothing safe to ask this peer about.
		case strings.Contains(pt.BaseDir, "'"):
			sshErr = fmt.Errorf("peer base_dir %q is not usable over ssh", pt.BaseDir)
		default:
			text, err := sshOut(pt.Peer.Target(), syncMeasureScript(pt.BaseDir, askable, cfg.Remotes()))
			if err != nil {
				sshErr = fmt.Errorf("%w: %s: %v", errPeerUnreachable, pt.Peer.Target(), err)
			} else {
				positions = parseSyncPositions(text)
			}
		}
		if sshErr != nil {
			errs = append(errs, sshErr)
		}

		for i, rel := range repos {
			var pos SyncPos
			switch {
			case badRel[rel]:
				pos = SyncPos{Err: "unsupported character (') in the repo path"}
			case sshErr != nil:
				pos = SyncPos{Err: sshErr.Error()}
			default:
				if p, ok := positions[rel]; ok {
					pos = p
				} else {
					// The remote script emits exactly one line per repo it was
					// asked about, so this should never happen for real - but
					// silence here must never be read as "level". A peer that
					// drops a repo (rev-list failed, a malformed line got
					// discarded by parseSyncPositions, ...) has to block that
					// repo, the same as any other unreadable answer, so it shows
					// up as a real warning instead of quietly looking synced.
					pos = SyncPos{Err: "the peer did not report on this repo"}
				}
			}
			out[i].There = append(out[i].There, PeerPos{Peer: pt.Peer, Pos: pos})
		}
	}
	return out, errors.Join(errs...)
}

// measureHere is MeasureSync's local half, mirroring exactly what the remote
// script does: same remote-resolution rule, same fetch, same counts.
func measureHere(cfg config.Config, rel string) SyncPos {
	dir := cfg.RepoPath(rel)
	branch, err := gitcmd.CurrentBranch(dir)
	if err != nil {
		return SyncPos{Err: "detached HEAD"}
	}
	remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
	if err != nil {
		return SyncPos{Branch: branch, Err: "no remote to sync through"}
	}
	if err := gitcmd.Fetch(dir, remote); err != nil {
		return SyncPos{Branch: branch, Remote: remote, Err: "fetch from " + remote + " failed"}
	}
	if !gitcmd.HasRemoteBranch(dir, remote, branch) {
		// Nothing to be level with yet. Pushing would create the branch, but
		// that is a decision about the shared remote's contents, not a repair.
		return SyncPos{Branch: branch, Remote: remote,
			Err: branch + " is not on " + remote + " yet"}
	}
	ahead, behind, err := gitcmd.AheadBehind(dir, remote, branch)
	if err != nil {
		return SyncPos{Branch: branch, Remote: remote, Err: "could not compare with " + remote}
	}
	return SyncPos{Branch: branch, Remote: remote, Ahead: ahead, Behind: behind}
}

// ApplySync repairs each repo: exactly one machine publishes - the first that
// is purely ahead, this machine before any peer - and every other machine
// then fast-forwards onto it. A second ahead machine is left alone and
// reported by the re-measure: pushing both would be a merge decision, and
// that is the user's.
//
// Returns the repos as measured again afterwards, so the caller reports what
// actually happened rather than what was intended.
func ApplySync(cfg config.Config, peers []PeerTarget, repos []RepoSync) []RepoSync {
	// Warnings that survive the re-measure: a repo can end up perfectly level
	// and still have left the user something to do.
	notes := map[string]string{}

	// Pass 1: the one publisher per repo. Repos are grouped by the machine
	// that will push them, so each peer needs at most one round trip.
	pushHere := []RepoSync{}
	pushThere := map[string][]RepoSync{} // keyed by peer host
	for _, r := range repos {
		if r.blocked() {
			continue
		}
		if r.Here.canPush() {
			pushHere = append(pushHere, r)
			continue
		}
		for _, pp := range r.There {
			if pp.Pos.canPush() {
				pushThere[pp.Peer.Host] = append(pushThere[pp.Peer.Host], r)
				break // the first ahead machine only
			}
		}
	}
	for _, r := range pushHere {
		_, _ = gitcmd.Push(cfg.RepoPath(r.Rel), r.Here.Remote, r.Here.Branch)
	}
	for _, pt := range peers {
		rs := pushThere[pt.Peer.Host]
		if len(rs) == 0 {
			continue
		}
		// Best effort: the re-measure below is what the user is shown, so an
		// ssh failure here surfaces as "still not level", never as a false
		// claim of success.
		_, _ = sshOut(pt.Peer.Target(), syncApplyScript(pt.BaseDir, relsOf(rs), cfg.Remotes()))
	}

	// Pass 2: everyone else lands what was just published. Every peer is
	// asked about every unblocked repo - the remote script already no-ops on
	// a repo with nothing to fast-forward.
	var landable []RepoSync
	for _, r := range repos {
		if !r.blocked() {
			landable = append(landable, r)
		}
	}
	for _, pt := range peers {
		if len(landable) == 0 {
			break
		}
		_, _ = sshOut(pt.Peer.Target(), syncApplyScript(pt.BaseDir, relsOf(landable), cfg.Remotes()))
	}
	for _, r := range landable {
		dir := cfg.RepoPath(r.Rel)
		if r.Here.Remote == "" || r.Here.Branch == "" {
			continue
		}
		if err := gitcmd.Fetch(dir, r.Here.Remote); err != nil {
			continue
		}
		ahead, behind, err := gitcmd.AheadBehind(dir, r.Here.Remote, r.Here.Branch)
		if err != nil || behind == 0 || ahead > 0 {
			continue // nothing to land, or diverged - never merged automatically
		}
		if note := landHere(dir, r.Here.Remote, r.Here.Branch); note != "" {
			notes[r.Rel] = note
		}
	}

	// Pass 3: re-measure, so the user is shown what happened rather than what
	// was intended, with the warnings that survive a successful sync.
	final, _ := MeasureSync(cfg, peers, relsOf(repos))
	for i := range final {
		if n, ok := notes[final[i].Rel]; ok {
			final[i].Here.Note = n
		}
	}
	return final
}

// landHere fast-forwards dir onto remote/branch, stashing first if the tree is
// dirty. This is deliberately the same dance receive.go performs on every
// sync, including popping unconditionally: a tree we stashed must come back
// whether or not the merge worked, or uncommitted work is silently hidden.
//
// Doing less than receive here would be its own trap - a repo left unlevelled
// at install time purely because a file was edited, when the very next commit
// would have handled it.
func landHere(dir, remote, branch string) (note string) {
	dirty, err := gitcmd.IsDirty(dir)
	if err != nil {
		return "could not read the working tree"
	}
	if dirty {
		if err := gitcmd.Stash(dir); err != nil {
			return "uncommitted changes could not be stashed, so this repo was left alone"
		}
		defer func() {
			// Unconditional, and its failure outranks whatever the merge had
			// to say: the changes still exist and the user has to be told
			// where they went.
			if err := gitcmd.StashPop(dir); err != nil {
				note = "your uncommitted changes are safe in `git stash list` " +
					"but conflicted on the way back - restore them by hand"
			}
		}()
	}
	if err := gitcmd.FastForward(dir, remote, branch); err != nil {
		return "fast-forward failed: " + firstLine(err.Error())
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func relsOf(repos []RepoSync) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Rel
	}
	return out
}

// RenderSyncPlan describes what ApplySync would do, and returns whether there
// is anything to do at all. Printed before acting: this is the one moment
// git-sync pushes commits the user did not just make, so it says so first.
func RenderSyncPlan(w io.Writer, repos []RepoSync) bool {
	var work []RepoSync
	for _, r := range repos {
		if r.needsWork() {
			work = append(work, r)
		}
	}
	if len(work) == 0 {
		fmt.Fprintf(w, "all %d selected repos are already level with their remotes\n", len(repos))
		return false
	}

	fmt.Fprintf(w, "%d of %d selected repos are not level with their remotes:\n", len(work), len(repos))
	for _, r := range work {
		fmt.Fprintf(w, "  %s\n", r.Rel)
		isHerePub, peerPub, _ := r.publisher()
		width := labelWidth(r)
		fmt.Fprintf(w, "      %s %s\n", padLabel("here:", width), describePos(r.Here, isHerePub))
		for _, pp := range r.There {
			fmt.Fprintf(w, "      %s %s\n", padLabel(pp.Peer.Host+":", width),
				describePos(pp.Pos, peerPub == pp.Peer.Host))
		}
		if r.blocked() {
			fmt.Fprintf(w, "            -> %s\n", blockedReason(r))
		}
	}
	return true
}

// labelWidth is the widest machine label in r ("here" or a peer's host), so
// every position line in the repo's block lines up under a common column.
func labelWidth(r RepoSync) int {
	w := len("here:")
	for _, p := range r.There {
		if l := len(p.Peer.Host) + 1; l > w {
			w = l
		}
	}
	return w
}

func padLabel(label string, width int) string {
	return fmt.Sprintf("%-*s", width, label)
}

// describePos describes one machine's position for the plan. isPublisher
// says whether ApplySync will actually push from here: a second machine that
// is also ahead is reported, not silently treated the same as the one that
// will publish, since pushing both would be a merge decision left to the
// user.
func describePos(p SyncPos, isPublisher bool) string {
	if !p.ok() {
		return p.Err
	}
	switch {
	case p.converged():
		return fmt.Sprintf("%s level with %s", p.Branch, p.Remote)
	case p.diverged():
		return fmt.Sprintf("%s diverged from %s: %d ahead, %d behind", p.Branch, p.Remote, p.Ahead, p.Behind)
	case p.Ahead > 0:
		if isPublisher {
			return fmt.Sprintf("%s is %d ahead of %s (will push)", p.Branch, p.Ahead, p.Remote)
		}
		return fmt.Sprintf("%s is %d ahead of %s  (left alone: another machine is ahead)", p.Branch, p.Ahead, p.Remote)
	default:
		return fmt.Sprintf("%s is %d behind %s (will fast-forward)", p.Branch, p.Behind, p.Remote)
	}
}

// blockedReason explains why blocked() is true for r, so the plan can say so
// before acting. Checked in the same order blocked() checks it.
func blockedReason(r RepoSync) string {
	if !r.Here.ok() {
		return "cannot sync this repo here: " + r.Here.Err
	}
	for _, p := range r.There {
		if !p.Pos.ok() {
			return "cannot sync this repo on " + p.Peer.Host + ": " + p.Pos.Err
		}
	}
	for _, p := range r.There {
		if p.Pos.Branch != r.Here.Branch {
			return fmt.Sprintf("different branches checked out (%s here, %s on %s); "+
				"they only sync while both are on the same branch", r.Here.Branch, p.Pos.Branch, p.Peer.Host)
		}
	}
	return "history has diverged; merge it by hand, git-sync will not merge for you"
}

// RenderSyncResult reports the state after the repair and returns how many
// repos are still not level. It always prints something: silence would read
// as "it did not look".
func RenderSyncResult(w io.Writer, repos []RepoSync) int {
	var left []RepoSync
	for _, r := range repos {
		if r.needsWork() {
			left = append(left, r)
		}
	}
	// Notes outlive the repair: a repo can end up perfectly level and still
	// have left a conflicted stash behind, on any machine.
	for _, r := range repos {
		if r.Here.Note != "" {
			fmt.Fprintf(w, "  %s (here): %s\n", r.Rel, r.Here.Note)
		}
		for _, p := range r.There {
			if p.Pos.Note != "" {
				fmt.Fprintf(w, "  %s (%s): %s\n", r.Rel, p.Peer.Host, p.Pos.Note)
			}
		}
	}
	if len(left) == 0 {
		fmt.Fprintf(w, "every machine is level on all %d selected repos\n", len(repos))
		return 0
	}
	fmt.Fprintf(w, "%d of %d repos still need you:\n", len(left), len(repos))
	for _, r := range left {
		fmt.Fprintf(w, "  %s\n", r.Rel)
		width := labelWidth(r)
		fmt.Fprintf(w, "      %s %s\n", padLabel("here:", width), describeLeftover(r.Here))
		for _, p := range r.There {
			fmt.Fprintf(w, "      %s %s\n", padLabel(p.Peer.Host+":", width), describeLeftover(p.Pos))
		}
		if r.blocked() {
			fmt.Fprintf(w, "      -> %s\n", blockedReason(r))
		}
	}
	fmt.Fprintln(w, "git-sync is installed and will sync these as soon as they are level; "+
		"until then every commit warns instead.")
	return len(left)
}

// describeLeftover explains one machine's position *after* the repair.
// Distinct from describePos, which predicts before acting: a repo that is
// merely still behind was not diverged, and calling it that would send the
// user looking for a merge conflict that does not exist.
func describeLeftover(p SyncPos) string {
	if !p.ok() {
		return p.Err
	}
	switch {
	case p.converged():
		return fmt.Sprintf("%s level with %s", p.Branch, p.Remote)
	case p.diverged():
		return fmt.Sprintf("%s diverged from %s: %d ahead, %d behind; merge it by hand",
			p.Branch, p.Remote, p.Ahead, p.Behind)
	case p.Ahead > 0:
		return fmt.Sprintf("still %d ahead of %s; the push did not go through", p.Ahead, p.Remote)
	default:
		return fmt.Sprintf("still %d behind %s; the fast-forward did not apply", p.Behind, p.Remote)
	}
}

// parseSyncPositions reads the remote script's output. Two line shapes:
//
//	pos <rel> <branch> <remote> <ahead> <behind>
//	err <rel> <message...>
func parseSyncPositions(text string) map[string]SyncPos {
	out := map[string]SyncPos{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(strings.TrimSpace(sc.Text()))
		if len(f) < 3 {
			continue
		}
		switch f[0] {
		case "err":
			out[f[1]] = SyncPos{Err: strings.Join(f[2:], " ")}
		case "note":
			// Arrives before the repo's pos line, so keep it and let the pos
			// case merge it in rather than overwrite it.
			p := out[f[1]]
			p.Note = strings.Join(f[2:], " ")
			out[f[1]] = p
		case "pos":
			if len(f) < 6 {
				continue
			}
			p := SyncPos{Branch: f[2], Remote: f[3], Note: out[f[1]].Note}
			if _, err := fmt.Sscan(f[4], &p.Ahead); err != nil {
				continue
			}
			if _, err := fmt.Sscan(f[5], &p.Behind); err != nil {
				continue
			}
			out[f[1]] = p
		}
	}
	return out
}

// syncMeasureScript is the shell mirror of measureHere: same remote
// preference order, same fetch, same counts. Kept in one string so the whole
// question is one ssh round trip.
func syncMeasureScript(peerBase string, repos, remotePrefs []string) string {
	return syncScript(peerBase, repos, remotePrefs, "")
}

// syncApplyScript measures the same way, then pushes if purely ahead and
// fast-forwards if purely behind - never both, and never a merge.
// The stash/pop around the merge mirrors landHere, which in turn mirrors
// receive: a dirty working tree must not be the reason a repo is left
// unlevelled, and a stashed tree comes back whether or not the merge worked.
func syncApplyScript(peerBase string, repos, remotePrefs []string) string {
	return syncScript(peerBase, repos, remotePrefs, `
  if [ "$ahead" -gt 0 ] && [ "$behind" -eq 0 ]; then
    git -C "$d" push -q "$r" "$b" >/dev/null 2>&1 || true
  fi
  git -C "$d" fetch -q "$r" >/dev/null 2>&1 || true
  set -- $(git -C "$d" rev-list --left-right --count "$r/$b...$b")
  if [ "$2" -eq 0 ] && [ "$1" -gt 0 ]; then
    stashed=no
    if [ -n "$(git -C "$d" status --porcelain)" ]; then
      git -C "$d" stash push -u -m 'git-sync initial sync' >/dev/null 2>&1 && stashed=yes
    fi
    git -C "$d" merge --ff-only "$r/$b" >/dev/null 2>&1 || true
    if [ "$stashed" = yes ] && ! git -C "$d" stash pop >/dev/null 2>&1; then
      echo "note $rel uncommitted changes are safe in the peer's git stash list but conflicted on the way back"
    fi
  fi`)
}

// syncScript builds the remote shell shared by measure and apply. `.git` is
// tested with -e, not -d: in a worktree or a submodule it is a file.
func syncScript(peerBase string, repos, remotePrefs []string, act string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nbase='%s'\nprefs=\"%s\"\nfor rel in",
		initialSyncMarker, peerBase, strings.Join(remotePrefs, " "))
	for _, rel := range repos {
		fmt.Fprintf(&b, " '%s'", rel)
	}
	b.WriteString(`; do
  d="$base/$rel"
  if [ ! -e "$d/.git" ]; then echo "err $rel not a git repo on this machine"; continue; fi
  b=$(git -C "$d" symbolic-ref --short HEAD 2>/dev/null) || { echo "err $rel detached HEAD"; continue; }
  r=""
  for p in $prefs; do
    if git -C "$d" remote get-url "$p" >/dev/null 2>&1; then r="$p"; break; fi
  done
  if [ -z "$r" ] && [ "$(git -C "$d" remote | wc -l | tr -d ' ')" = "1" ]; then r=$(git -C "$d" remote); fi
  if [ -z "$r" ]; then echo "err $rel no remote to sync through"; continue; fi
  if ! git -C "$d" fetch -q "$r" >/dev/null 2>&1; then echo "err $rel fetch from $r failed"; continue; fi
  if ! git -C "$d" rev-parse --verify --quiet "$r/$b" >/dev/null; then
    echo "err $rel $b is not on $r yet"; continue
  fi
  set -- $(git -C "$d" rev-list --left-right --count "$r/$b...$b")
  behind=$1; ahead=$2
` + act + `
  set -- $(git -C "$d" rev-list --left-right --count "$r/$b...$b")
  echo "pos $rel $b $r $2 $1"
done
`)
	return b.String()
}
