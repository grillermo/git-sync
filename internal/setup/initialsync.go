package setup

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
)

// Initial synchronisation: get both machines level with the shared remote
// *before* the hook is armed, so the first commit after install is an ordinary
// fast-forward on the other side.
//
// Without this step, a repo that was already out of step at install time stays
// broken silently. The failure looks exactly like the one this package exists
// to prevent: the pusher reports "pushed" and "peer synced", while the peer's
// receive refuses the fast-forward and warns into a log nobody is watching. A
// machine holding one unpushed commit from last week is enough to cause it,
// and it never resolves on its own, because receive never pushes.
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

// RepoSync is one selected repo measured on both machines.
type RepoSync struct {
	Rel   string
	Here  SyncPos
	There SyncPos
}

// needsWork reports whether this repo is anything other than fully healthy -
// either it can be repaired, or it needs the user. Blocked counts even when
// both sides are individually converged: two machines sitting level on
// *different branches* each look fine alone, and the pair still never syncs.
func (r RepoSync) needsWork() bool {
	return r.blocked() || !r.Here.converged() || !r.There.converged()
}

// blocked reports whether push and fast-forward cannot fix this repo, so the
// user has to. A branch mismatch counts: the two machines are not meeting on
// the same branch, and nothing we are willing to do changes that.
func (r RepoSync) blocked() bool {
	if !r.Here.ok() || !r.There.ok() {
		return true
	}
	if r.Here.diverged() || r.There.diverged() {
		return true
	}
	return r.Here.Branch != r.There.Branch
}

// initialSyncMarker opens the remote scripts, so they are identifiable in the
// debug log and in the ssh stub's recorded calls.
const initialSyncMarker = "# git-sync-initial-sync"

// MeasureSync reports, for each repo, how far each machine is from the shared
// remote. It fetches on both sides first: the question is about the remote as
// it is now, not as it was at the last fetch.
//
// Measuring is read-only, so it is safe to run before asking the user anything.
// PlanSync turns the result into a description, ApplySync acts on it.
func MeasureSync(target, peerBase string, cfg config.Config, repos []string) ([]RepoSync, error) {
	out := make([]RepoSync, 0, len(repos))
	askable := make([]string, 0, len(repos))
	for _, rel := range repos {
		rs := RepoSync{Rel: rel, Here: measureHere(cfg, rel)}
		if strings.Contains(rel, "'") {
			// Cannot be named safely in the remote shell command, and a repo we
			// cannot ask about must not be reported as fine.
			rs.There = SyncPos{Err: "unsupported character (') in the repo path"}
		} else {
			askable = append(askable, rel)
		}
		out = append(out, rs)
	}

	if len(askable) == 0 || strings.Contains(peerBase, "'") {
		return out, nil
	}
	for _, name := range cfg.Remotes() {
		if strings.Contains(name, "'") {
			return out, fmt.Errorf("remote name %q is not usable over ssh", name)
		}
	}

	text, err := sshOut(target, syncMeasureScript(peerBase, askable, cfg.Remotes()))
	if err != nil {
		return out, fmt.Errorf("%w: %s: %v", errPeerUnreachable, target, err)
	}
	byRel := parseSyncPositions(text)
	for i := range out {
		if out[i].There.Err != "" {
			continue // already excluded above
		}
		p, ok := byRel[out[i].Rel]
		if !ok {
			// Fold in by name rather than by line order, so a dropped line
			// leaves that one repo unmeasured instead of shifting every later
			// answer onto the wrong repo.
			p = SyncPos{Err: "the peer did not report on this repo"}
		}
		out[i].There = p
	}
	return out, nil
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

// ApplySync performs the repair: every machine that is purely ahead pushes,
// then every machine that is purely behind fast-forwards. Deliberately in that
// order and in two passes - a side can only fast-forward onto what the other
// side has already pushed.
//
// Returns the repos as measured again afterwards, so the caller reports what
// actually happened rather than what was intended.
func ApplySync(target, peerBase string, cfg config.Config, repos []RepoSync) []RepoSync {
	// Warnings that survive the re-measure: a repo can end up perfectly level
	// and still have left the user something to do.
	notes := map[string]string{}

	// Pass 1: this machine publishes, so the peer has something to land on.
	// A failed push is not handled here: pass 3 re-measures, and a repo that
	// did not move simply reports as still not level.
	for _, r := range repos {
		if !r.blocked() && r.Here.canPush() {
			_, _ = gitcmd.Push(cfg.RepoPath(r.Rel), r.Here.Remote, r.Here.Branch)
		}
	}

	// Pass 2: the peer publishes and lands, in one round trip. It runs after
	// our push, so its fast-forward sees the commits we just sent.
	var peerRepos []RepoSync
	for _, r := range repos {
		if !r.blocked() && (r.There.canPush() || r.There.canFF() || r.Here.canPush()) {
			peerRepos = append(peerRepos, r)
		}
	}
	if len(peerRepos) > 0 && !strings.Contains(peerBase, "'") {
		rels := make([]string, 0, len(peerRepos))
		for _, r := range peerRepos {
			rels = append(rels, r.Rel)
		}
		// Best effort: the re-measure below is what the user is shown, so an
		// ssh failure here surfaces as "still not level", not as a false claim.
		_, _ = sshOut(target, syncApplyScript(peerBase, rels, cfg.Remotes()))
	}

	// Pass 3: this machine lands whatever the peer just published. Repos that
	// only needed a local fast-forward are included, as are those that became
	// landable during pass 2.
	for _, r := range repos {
		if r.blocked() {
			continue
		}
		dir := cfg.RepoPath(r.Rel)
		if r.Here.Remote == "" || r.Here.Branch == "" {
			continue
		}
		if err := gitcmd.Fetch(dir, r.Here.Remote); err != nil {
			continue
		}
		ahead, behind, err := gitcmd.AheadBehind(dir, r.Here.Remote, r.Here.Branch)
		if err != nil || behind == 0 || ahead > 0 {
			// Nothing to land, or diverged - and a diverged tree is never
			// merged automatically.
			continue
		}
		if note := landHere(dir, r.Here.Remote, r.Here.Branch); note != "" {
			notes[r.Rel] = note
		}
	}

	final, _ := MeasureSync(target, peerBase, cfg, relsOf(repos))
	// On an ssh failure MeasureSync still returns a row per repo, each with
	// its own Err, so an unmeasurable peer renders as unresolved rather than
	// as a sync that silently reported nothing.
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
func RenderSyncPlan(w io.Writer, peerHost string, repos []RepoSync) bool {
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
		fmt.Fprintf(w, "      here: %s\n", describePos(r.Here))
		fmt.Fprintf(w, "      %-4s: %s\n", "peer", describePos(r.There))
		if !r.blocked() {
			continue
		}
		fmt.Fprintf(w, "            -> %s\n", blockedReason(r, peerHost))
	}
	return true
}

func describePos(p SyncPos) string {
	if !p.ok() {
		return p.Err
	}
	switch {
	case p.converged():
		return fmt.Sprintf("%s level with %s", p.Branch, p.Remote)
	case p.diverged():
		return fmt.Sprintf("%s diverged from %s: %d ahead, %d behind", p.Branch, p.Remote, p.Ahead, p.Behind)
	case p.Ahead > 0:
		return fmt.Sprintf("%s is %d ahead of %s (will push)", p.Branch, p.Ahead, p.Remote)
	default:
		return fmt.Sprintf("%s is %d behind %s (will fast-forward)", p.Branch, p.Behind, p.Remote)
	}
}

func blockedReason(r RepoSync, peerHost string) string {
	switch {
	case !r.Here.ok():
		return "cannot sync this repo here: " + r.Here.Err
	case !r.There.ok():
		return "cannot sync this repo on " + peerHost + ": " + r.There.Err
	case r.Here.Branch != r.There.Branch:
		return fmt.Sprintf("different branches checked out (%s here, %s on %s); "+
			"they only sync while both are on the same branch", r.Here.Branch, r.There.Branch, peerHost)
	default:
		return "history has diverged; merge it by hand, git-sync will not merge for you"
	}
}

// RenderSyncResult reports the state after the repair and returns how many
// repos are still not level. It always prints something: silence would read as
// "it did not look".
func RenderSyncResult(w io.Writer, peerHost string, repos []RepoSync) int {
	var left []RepoSync
	for _, r := range repos {
		if r.needsWork() {
			left = append(left, r)
		}
	}
	// Notes outlive the repair: a repo can be perfectly level and still have
	// left a conflicted stash behind.
	for _, r := range repos {
		for who, note := range map[string]string{"here": r.Here.Note, peerHost: r.There.Note} {
			if note != "" {
				fmt.Fprintf(w, "  %s (%s): %s\n", r.Rel, who, note)
			}
		}
	}
	if len(left) == 0 {
		fmt.Fprintf(w, "both machines are level on all %d selected repos\n", len(repos))
		return 0
	}
	fmt.Fprintf(w, "%d of %d repos still need you:\n", len(left), len(repos))
	for _, r := range left {
		fmt.Fprintf(w, "  %-24s %s\n", r.Rel, leftoverReason(r, peerHost))
	}
	fmt.Fprintln(w, "git-sync is installed and will sync these as soon as they are level; "+
		"until then every commit warns instead.")
	return len(left)
}

// leftoverReason explains why a repo is still not level *after* the repair.
// Distinct from blockedReason, which predicts before acting: a repo that is
// merely still behind was not diverged, and saying so would send the user
// looking for a merge conflict that does not exist.
func leftoverReason(r RepoSync, peerHost string) string {
	if r.blocked() {
		return blockedReason(r, peerHost)
	}
	switch {
	case r.Here.Ahead > 0:
		return fmt.Sprintf("still %d ahead of %s here; the push did not go through",
			r.Here.Ahead, r.Here.Remote)
	case r.Here.Behind > 0:
		return fmt.Sprintf("still %d behind %s here; the fast-forward did not apply",
			r.Here.Behind, r.Here.Remote)
	case r.There.Ahead > 0:
		return fmt.Sprintf("%s is still %d ahead of %s; its push did not go through",
			peerHost, r.There.Ahead, r.There.Remote)
	case r.There.Behind > 0:
		return fmt.Sprintf("%s is still %d behind %s; its fast-forward did not apply",
			peerHost, r.There.Behind, r.There.Remote)
	default:
		return "not level, for a reason git-sync could not determine"
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
