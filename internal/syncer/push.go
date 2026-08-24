package syncer

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
)

// ExitRepoNotHere is receive's exit code for "this machine has no copy of
// that repo". ssh propagates a remote command's exit status, so push can tell
// that apart from a real failure.
const ExitRepoNotHere = 3

// Push pushes rel to the shared remote and then tells the peer to pull it.
// Runs detached in the background; its only output is the activity log.
// Returns a process exit code.
func Push(rel string) int {
	cfg, err := config.Load()
	if err != nil {
		return 1
	}
	if err := cfg.ValidateRel(rel); err != nil {
		activity.AppendDebug("push: " + err.Error())
		return 1
	}
	// The relpath goes into a single-quoted remote command below, so a single
	// quote in it would break out of that quoting.
	if strings.Contains(rel, "'") {
		activity.AppendDebug("push: unsupported character (') in " + rel)
		return 1
	}

	dir := cfg.RepoPath(rel)

	// Everything below needs a branch to name on the remote, so a detached
	// HEAD stops here rather than half-syncing.
	branch, err := gitcmd.CurrentBranch(dir)
	if err != nil {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpPush, Status: activity.StatusSkip,
			Msg: "detached HEAD, nothing to push",
		})
		return 0
	}

	// The remote is the transport: no remote, no sync, and no point telling
	// the peer to pull. A warning rather than a skip - the repo is selected,
	// so the user believes it is syncing and it is not.
	remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
	if err != nil {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpPush, Status: activity.StatusWarn,
			Branch: branch, Msg: "no remote to sync through: " + firstLine(err.Error()),
		})
		return 0
	}

	// No retry queue, by design: if we are offline or the push is rejected,
	// the next commit pushes both commits anyway.
	if _, err := gitcmd.Push(dir, remote, branch); err != nil {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpPush, Status: activity.StatusError,
			Branch: branch, Msg: "push to " + remote + " failed: " + firstLine(err.Error()),
		})
		return 0
	}
	_ = activity.Append(activity.Event{
		Repo: rel, Op: activity.OpPush, Status: activity.StatusOK,
		Branch: branch, Msg: "pushed " + branch + " to " + remote,
	})

	notify(cfg, rel, branch)
	return 0
}

// notify asks the peer to run its own receive for this repo.
//
// Task 12 replaces the exec.Command below with sshx.Command, so that a peer
// which needs a password works here too. Until then this is key-auth only.
func notify(cfg config.Config, rel, branch string) {
	target := cfg.PeerUser + "@" + cfg.PeerHost
	remote := fmt.Sprintf("~/.gitsync/bin/git-sync receive '%s'", rel)

	cmd := exec.Command("ssh",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		target, remote)
	out, err := cmd.CombinedOutput()

	ev := activity.Event{Repo: rel, Op: activity.OpNotify, Branch: branch, Peer: cfg.PeerHost}
	switch code := exitCode(err); {
	case err == nil:
		ev.Status, ev.Msg = activity.StatusOK, "peer "+cfg.PeerHost+" synced"
	case code == ExitRepoNotHere:
		// Expected and harmless: the peer has never cloned this repo. No
		// auto-clone, no retry - just say so plainly rather than claiming a
		// sync that never happened.
		ev.Status, ev.Msg = activity.StatusSkip, "peer has no copy of this repo, nothing to sync"
	case code == 255:
		ev.Status, ev.Msg = activity.StatusError, "peer "+cfg.PeerHost+" unreachable"
	default:
		ev.Status = activity.StatusError
		ev.Msg = fmt.Sprintf("peer receive failed (exit %d)", code)
	}
	_ = activity.Append(ev)
	if err != nil {
		activity.AppendDebug("ssh " + target + ": " + strings.TrimSpace(string(out)))
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
