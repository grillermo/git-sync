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

// syncRepo is the spec's algorithm, inside the lock.
func syncRepo(cfg config.Config, rel, dir string) int {
	log := func(s activity.Status, branch, msg string) {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpReceive, Status: s, Branch: branch, Msg: msg,
		})
	}

	branch, err := gitcmd.CurrentBranch(dir)
	if err != nil {
		log(activity.StatusSkip, "", "detached HEAD, skipping")
		return 0
	}

	// Resolve the remote by the same rule push used, rather than following
	// this branch's @{upstream}: the upstream may point at a different remote
	// from the one the commits were pushed to, and then we would fetch a
	// repository the peer never wrote to and report "up to date" forever.
	remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
	if err != nil {
		log(activity.StatusWarn, branch, "no remote to sync through: "+firstLine(err.Error()))
		return 0
	}

	if err := gitcmd.Fetch(dir, remote); err != nil {
		log(activity.StatusError, branch, "fetch from "+remote+" failed: "+firstLine(err.Error()))
		return 0
	}

	// Checked before the merge: a missing remote-tracking ref would otherwise
	// fail the merge and be misreported as "diverged". Normal when the two
	// machines have different branches checked out.
	if !gitcmd.HasRemoteBranch(dir, remote, branch) {
		log(activity.StatusSkip, branch, branch+" is not on the remote "+remote+", skipping")
		return 0
	}

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

	if err := gitcmd.FastForward(dir, remote, branch); err != nil {
		// The fetch already updated the remote-tracking refs, so the user has
		// everything they need locally to merge by hand.
		log(activity.StatusWarn, branch, "diverged from "+remote+"/"+branch+
			", fetched only, manual merge needed")
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
