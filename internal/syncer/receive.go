package syncer

import (
	"os"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/lock"
)

// Receive applies whatever the peer just pushed to the local copy of rel.
// Invoked remotely, over ssh, by the peer's push.
//
// It never commits and never pushes, so it cannot re-trigger the peer's
// post-commit hook: there is no feedback loop between the two machines.
//
// Returns a process exit code; ExitRepoNotHere when this machine has no copy.
func Receive(rel string) int {
	cfg, err := config.Load()
	if err != nil {
		return 1
	}
	if err := cfg.ValidateRel(rel); err != nil {
		activity.AppendDebug("receive: " + err.Error())
		return 1
	}

	dir := cfg.RepoPath(rel)
	// Stat .git rather than the directory: in a linked worktree or a
	// submodule .git is a file, and that is still a real repo. Checked before
	// IsSelected so a repo that is simply absent from this machine gets the
	// more specific "not on this machine" message rather than "not selected".
	if _, err := os.Stat(dir + "/.git"); err != nil {
		// No auto-clone by design: first-time setup on a machine is a manual
		// clone. Expected for any repo that only exists on the other side.
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpReceive, Status: activity.StatusSkip,
			Msg: "not on this machine, skipping",
		})
		return ExitRepoNotHere
	}

	// install keeps both machines' lists in step, so this normally cannot
	// happen. It catches drift - a hand-edited config, or a peer provisioned
	// before a later selection change - and turns the disagreement into the
	// pusher's existing harmless no-op rather than a surprise sync.
	if !cfg.IsSelected(rel) {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpReceive, Status: activity.StatusSkip,
			Msg: "not selected for syncing on this machine",
		})
		return ExitRepoNotHere
	}

	l, err := lock.Acquire(rel, lockTimeout())
	if err != nil {
		if lock.IsBusy(err) {
			// Safe to drop: the holder brings the repo fully up to date.
			_ = activity.Append(activity.Event{
				Repo: rel, Op: activity.OpReceive, Status: activity.StatusSkip,
				Msg: "sync already in progress, giving up",
			})
			return 0
		}
		return 1
	}
	defer l.Release()

	return syncRepo(cfg, rel, dir)
}

// syncRepo is the spec's algorithm, inside the lock. It anchors to the
// remote's default branch, resolved fresh every call, rather than whatever is
// checked out here: that is where the two machines meet, independent of a
// feature branch or detached HEAD on either side.
func syncRepo(cfg config.Config, rel, dir string) int {
	log := func(s activity.Status, branch, msg string) {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpReceive, Status: s, Branch: branch, Msg: msg,
		})
	}

	// Resolve the remote by the same rule push used, rather than following
	// any branch's @{upstream}: the upstream may point at a different remote
	// from the one the commits were pushed to, and then we would fetch a
	// repository the peer never wrote to and report "up to date" forever.
	remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
	if err != nil {
		log(activity.StatusWarn, "", "no remote to sync through: "+firstLine(err.Error()))
		return 0
	}

	branch, err := gitcmd.DefaultBranch(dir, remote)
	if err != nil {
		log(activity.StatusWarn, "", "could not resolve default branch on "+remote+": "+firstLine(err.Error()))
		return 0
	}

	// A repo with only feature branches and no local default branch is
	// skipped, never auto-created or checked out.
	if !gitcmd.HasLocalBranch(dir, branch) {
		log(activity.StatusSkip, branch, "no local "+branch+" branch, skipping")
		return 0
	}

	if err := gitcmd.Fetch(dir, remote); err != nil {
		log(activity.StatusError, branch, "fetch from "+remote+" failed: "+firstLine(err.Error()))
		return 0
	}

	// Checked before measuring: a missing remote-tracking ref would otherwise
	// fail ahead/behind and be misreported as "diverged". Normal when the
	// remote's default branch was renamed or removed since the last clone.
	if !gitcmd.HasRemoteBranch(dir, remote, branch) {
		log(activity.StatusSkip, branch, branch+" is not on the remote "+remote+", skipping")
		return 0
	}

	ahead, behind, err := gitcmd.AheadBehind(dir, remote, branch)
	if err != nil {
		log(activity.StatusError, branch, "could not compare with "+remote+"/"+branch+": "+firstLine(err.Error()))
		return 0
	}

	if behind == 0 {
		log(activity.StatusOK, branch, branch+" already up to date with "+remote)
		return 0
	}

	if ahead > 0 {
		// The fetch already updated the remote-tracking refs, so the user has
		// everything they need locally to merge by hand. Receive stays
		// fast-forward-only in steady state: auto-merging here would let
		// both machines mint rival merge commits that never converge, and
		// receive never pushes so it could not fix that itself.
		log(activity.StatusWarn, branch, "diverged from "+remote+"/"+branch+
			", fetched only, manual merge needed")
		return 0
	}

	head, _ := gitcmd.CurrentBranch(dir) // "" on detached HEAD
	if head != branch {
		// Not checked out here: advance the ref directly. No worktree, so no
		// stash is ever needed.
		if err := gitcmd.FastForwardRef(dir, remote, branch); err != nil {
			// AheadBehind already established ahead==0, so this should
			// always succeed; IsNotFastForward is a defensive fallback in
			// case the local branch moved between the two checks, and it
			// gets the same "manual merge" wording divergence does elsewhere.
			if gitcmd.IsNotFastForward(err) {
				log(activity.StatusWarn, branch, "diverged from "+remote+"/"+branch+
					", fetched only, manual merge needed")
			} else {
				log(activity.StatusError, branch, "could not fast-forward "+branch+": "+firstLine(err.Error()))
			}
		} else {
			log(activity.StatusOK, branch, "fast-forwarded "+branch+" from "+remote)
		}
		return 0
	}

	// The default branch is checked out here: the stash/merge/pop dance.
	// Ask git directly whether the tree is dirty. Never infer it from a
	// pull's exit code, which cannot tell a dirty tree from diverged history.
	dirty, err := gitcmd.IsDirty(dir)
	if err != nil {
		log(activity.StatusError, branch, "could not read status: "+firstLine(err.Error()))
		return 0
	}
	stashed := false
	if dirty {
		if err := gitcmd.Stash(dir); err != nil {
			log(activity.StatusError, branch, "stash failed, not touching this repo")
			return 0
		}
		stashed = true
		log(activity.StatusOK, branch, "stashed dirty working tree")
	}

	// ahead==0 && behind>0 was already established above, so this merge is
	// guaranteed to be a fast-forward; a failure here is a genuine error, not
	// a divergence.
	if err := gitcmd.FastForward(dir, remote, branch); err != nil {
		log(activity.StatusError, branch, "fast-forward merge failed: "+firstLine(err.Error()))
	} else {
		log(activity.StatusOK, branch, "fast-forwarded "+branch+" from "+remote)
	}

	// Unconditional: whether or not the merge worked, a tree we stashed must
	// come back, or the user's uncommitted work is silently hidden away.
	if stashed {
		if err := gitcmd.StashPop(dir); err != nil {
			log(activity.StatusWarn, branch,
				"stash pop conflicted - resolve by hand; your changes are safe in `git stash list`")
		} else {
			log(activity.StatusOK, branch, "restored stashed changes")
		}
	}
	return 0
}

// lockTimeout allows tests to shorten the wait. Defaults to the spec's 30s.
func lockTimeout() time.Duration {
	if s := os.Getenv("GITSYNC_LOCK_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return lock.DefaultTimeout
}
