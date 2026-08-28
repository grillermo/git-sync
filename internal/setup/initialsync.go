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
// The repair uses mostly the two operations the steady state already relies
// on - push, and merge --ff-only - so it can add nothing to history that a
// normal sync would not. The one exception is install-time only: a genuinely
// diverged default branch, checked out on the diverged side, gets one real
// merge attempt; a clean merge is pushed and the other side fast-forwards
// onto it, a conflicting one is aborted and left for the user exactly as
// before. Steady-state receive never does this - see landMerge and
// ApplySync for why.

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
// the same branch, and nothing we are willing to do changes that. Kept as a
// defensive guard even though both sides now resolve the same remote's
// default branch the same way, so a mismatch should almost never happen.
func (r RepoSync) blocked() bool {
	if !r.Here.ok() || !r.There.ok() {
		return true
	}
	if r.Here.diverged() || r.There.diverged() {
		return true
	}
	return r.Here.Branch != r.There.Branch
}

// reachable reports whether ApplySync may attempt anything for this repo at
// all: both sides must be measurable and resolve to the same default branch.
// Divergence is deliberately not part of this check - since the install-time
// clean-merge step was added, a diverged repo is still worth attempting,
// just routed to landMerge (here) or the shell's mirror (on the peer)
// instead of a plain push or fast-forward.
func (r RepoSync) reachable() bool {
	return r.Here.ok() && r.There.ok() && r.Here.Branch == r.There.Branch
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
// script does: same remote-resolution rule, same default-branch resolution,
// same fetch, same counts. Anchored on the remote's default branch rather
// than whatever is checked out here - that is where the two machines meet,
// independent of a feature branch or detached HEAD on either side.
func measureHere(cfg config.Config, rel string) SyncPos {
	dir := cfg.RepoPath(rel)
	remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
	if err != nil {
		return SyncPos{Err: "no remote to sync through"}
	}
	branch, err := gitcmd.DefaultBranch(dir, remote)
	if err != nil {
		return SyncPos{Remote: remote, Err: "could not resolve default branch on " + remote}
	}
	if !gitcmd.HasLocalBranch(dir, branch) {
		// A repo with only feature branches and no local default branch is
		// skipped, never auto-created or checked out.
		return SyncPos{Branch: branch, Remote: remote, Err: "no local " + branch + " branch"}
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
// every genuinely diverged machine with its default branch checked out
// attempts one clean merge, then every machine that is purely behind
// fast-forwards. Deliberately in that order and across several passes - a
// side can only fast-forward onto what the other side has already pushed or
// merged.
//
// The clean-merge step is install-time only. Steady-state receive never does
// this: it stays fast-forward-only forever, because auto-merging there would
// let both machines mint rival merge commits that never converge, and
// receive never pushes so it could not fix that itself. Here, only the
// diverged side ever merges, so the two machines cannot mint rival merges of
// their own either.
//
// Returns the repos as measured again afterwards, so the caller reports what
// actually happened rather than what was intended.
func ApplySync(target, peerBase string, cfg config.Config, repos []RepoSync) []RepoSync {
	// Warnings that survive the re-measure: a repo can end up perfectly level
	// and still have left the user something to do.
	notes := map[string]string{}

	// Repos this machine just pushed something new to, whether via a plain
	// push or a successful merge. Tracked so pass 2 asks the peer about them
	// even though their pre-repair measurement did not show anything for the
	// peer to land - a landMerge push is invisible to the stale r.There value.
	pushedHere := map[string]bool{}

	// Pass 1: this machine publishes, so the peer has something to land on.
	// A failed push is not handled here: pass 3 re-measures, and a repo that
	// did not move simply reports as still not level.
	for _, r := range repos {
		if r.reachable() && r.Here.canPush() {
			if _, err := gitcmd.Push(cfg.RepoPath(r.Rel), r.Here.Remote, r.Here.Branch); err == nil {
				pushedHere[r.Rel] = true
			}
		}
	}

	// Pass 1b: this machine's genuinely diverged repos attempt one real
	// merge, but only when the default branch is checked out - merging a
	// branch that is not HEAD needs a throwaway worktree, not worth it here.
	// Runs before pass 2, so a clean merge's push is visible when the peer's
	// ssh round trip asks about the remote.
	for _, r := range repos {
		if !r.reachable() || !r.Here.diverged() {
			continue
		}
		dir := cfg.RepoPath(r.Rel)
		head, _ := gitcmd.CurrentBranch(dir)
		if head != r.Here.Branch {
			// Not checked out here: leave it diverged and blocked, exactly as
			// design decision 5 calls for. Nothing to do without a worktree.
			continue
		}
		note, pushed := landMerge(dir, r.Here.Remote, r.Here.Branch)
		if pushed {
			pushedHere[r.Rel] = true
		}
		if note != "" {
			notes[r.Rel] = note
		}
	}

	// Pass 2: the peer publishes and lands, in one round trip. It runs after
	// our push and merge, so its fast-forward (or its own clean merge) sees
	// the commits we just sent.
	var peerRepos []RepoSync
	for _, r := range repos {
		if !r.reachable() {
			continue
		}
		if r.There.canPush() || r.There.canFF() || r.Here.canPush() || r.There.diverged() || pushedHere[r.Rel] {
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

	// Pass 3: this machine lands whatever the peer just published, including
	// a merge commit the peer's diverged side just pushed. Repos that only
	// needed a local fast-forward are included, as are those that became
	// landable during pass 2.
	for _, r := range repos {
		if !r.reachable() {
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
			// Nothing to land, or still diverged - a diverged tree is never
			// merged automatically here; that was pass 1b's one attempt.
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

// landMerge attempts the one real merge install-time convergence is allowed
// to make: dir's default branch, which is genuinely diverged from
// remote/branch and checked out here, merged with remote/branch. A clean
// merge is pushed immediately, so pass 2's ssh round trip to the peer sees it
// as a plain fast-forward rather than a divergence of its own. A conflicting
// merge is aborted - git-sync never resolves a conflict - and the repo is
// left diverged and blocked for the user, exactly as it would have been
// without this step.
//
// Stashes first if the tree is dirty and pops unconditionally, mirroring
// landHere's discipline: a tree stashed here must come back whether or not
// the merge worked, and a pop failure outranks whatever the merge had to
// say, because the user needs to know where their uncommitted work went.
func landMerge(dir, remote, branch string) (note string, pushed bool) {
	dirty, err := gitcmd.IsDirty(dir)
	if err != nil {
		return "could not read the working tree", false
	}
	if dirty {
		if err := gitcmd.Stash(dir); err != nil {
			return "uncommitted changes could not be stashed, so this repo was left alone", false
		}
		defer func() {
			if err := gitcmd.StashPop(dir); err != nil {
				note = "your uncommitted changes are safe in `git stash list` " +
					"but conflicted on the way back - restore them by hand"
			}
		}()
	}
	if err := gitcmd.Merge(dir, remote, branch); err != nil {
		_ = gitcmd.MergeAbort(dir)
		return "history had diverged; the automatic merge conflicted, so it was aborted - " +
			"merge it by hand, git-sync will not merge for you", false
	}
	if _, err := gitcmd.Push(dir, remote, branch); err != nil {
		return "merged " + branch + " locally but could not push it to " + remote, false
	}
	return "", true
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
		return fmt.Sprintf("different branches resolved as default (%s here, %s on %s); "+
			"they only sync while both resolve to the same default branch", r.Here.Branch, r.There.Branch, peerHost)
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

// syncApplyScript measures the same way, then pushes if purely ahead,
// fast-forwards (or, if the default branch is not checked out, advances the
// ref directly) if purely behind, and - new - attempts one clean merge if
// genuinely diverged and the default branch is checked out. Mirrors
// landMerge: a clean merge is pushed immediately, a conflicting one is
// aborted and left for the user. The stash/pop around each mutation mirrors
// landHere, which in turn mirrors receive: a dirty working tree must not be
// the reason a repo is left unlevelled, and a stashed tree comes back
// whether or not the merge worked.
func syncApplyScript(peerBase string, repos, remotePrefs []string) string {
	return syncScript(peerBase, repos, remotePrefs, `
  if [ "$ahead" -gt 0 ] && [ "$behind" -eq 0 ]; then
    git -C "$d" push -q "$r" "$b" >/dev/null 2>&1 || true
  fi
  git -C "$d" fetch -q "$r" >/dev/null 2>&1 || true
  set -- $(git -C "$d" rev-list --left-right --count "$r/$b...$b")
  head=$(git -C "$d" symbolic-ref --short HEAD 2>/dev/null)
  if [ "$2" -eq 0 ] && [ "$1" -gt 0 ]; then
    if [ "$head" = "$b" ]; then
      stashed=no
      if [ -n "$(git -C "$d" status --porcelain)" ]; then
        git -C "$d" stash push -u -m 'git-sync initial sync' >/dev/null 2>&1 && stashed=yes
      fi
      git -C "$d" merge --ff-only "$r/$b" >/dev/null 2>&1 || true
      if [ "$stashed" = yes ] && ! git -C "$d" stash pop >/dev/null 2>&1; then
        echo "note $rel uncommitted changes are safe in the peer's git stash list but conflicted on the way back"
      fi
    else
      git -C "$d" update-ref "refs/heads/$b" "$r/$b" >/dev/null 2>&1 || true
    fi
  elif [ "$2" -gt 0 ] && [ "$1" -gt 0 ] && [ "$head" = "$b" ]; then
    stashed=no
    if [ -n "$(git -C "$d" status --porcelain)" ]; then
      git -C "$d" stash push -u -m 'git-sync initial sync' >/dev/null 2>&1 && stashed=yes
    fi
    if git -C "$d" merge --no-edit "$r/$b" >/dev/null 2>&1; then
      git -C "$d" push -q "$r" "$b" >/dev/null 2>&1 || true
    else
      git -C "$d" merge --abort >/dev/null 2>&1 || true
    fi
    if [ "$stashed" = yes ] && ! git -C "$d" stash pop >/dev/null 2>&1; then
      echo "note $rel uncommitted changes are safe in the peer's git stash list but conflicted on the way back"
    fi
  fi`)
}

// syncScript builds the remote shell shared by measure and apply. `.git` is
// tested with -e, not -d: in a worktree or a submodule it is a file. The
// remote is resolved before the branch, because resolving the default
// branch needs to know which remote's HEAD to read - the same order
// measureHere uses.
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
  r=""
  for p in $prefs; do
    if git -C "$d" remote get-url "$p" >/dev/null 2>&1; then r="$p"; break; fi
  done
  if [ -z "$r" ] && [ "$(git -C "$d" remote | wc -l | tr -d ' ')" = "1" ]; then r=$(git -C "$d" remote); fi
  if [ -z "$r" ]; then echo "err $rel no remote to sync through"; continue; fi
  if ! git -C "$d" fetch -q "$r" >/dev/null 2>&1; then echo "err $rel fetch from $r failed"; continue; fi
  b=$(git -C "$d" symbolic-ref --short "refs/remotes/$r/HEAD" 2>/dev/null) \
    || { git -C "$d" remote set-head "$r" -a >/dev/null 2>&1; \
         b=$(git -C "$d" symbolic-ref --short "refs/remotes/$r/HEAD" 2>/dev/null); }
  b=${b#$r/}
  if [ -z "$b" ]; then echo "err $rel could not resolve default branch"; continue; fi
  if ! git -C "$d" rev-parse --verify --quiet "refs/heads/$b" >/dev/null; then
    echo "err $rel no local $b branch"; continue
  fi
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
