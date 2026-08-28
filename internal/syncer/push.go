package syncer

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/sshx"
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

	// The remote is the transport: no remote, no sync, and no point telling
	// the peer to pull. A warning rather than a skip - the repo is selected,
	// so the user believes it is syncing and it is not.
	remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
	if err != nil {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpPush, Status: activity.StatusWarn,
			Msg: "no remote to sync through: " + firstLine(err.Error()),
		})
		return 0
	}

	// Always the remote's default branch, resolved fresh, never whatever is
	// checked out here: that is where the two machines meet, independent of a
	// feature branch or detached HEAD on either side. A commit on a
	// non-default branch simply leaves this branch unmoved, so pushing it is
	// a harmless no-op - that is how "non-default branches don't sync" falls
	// out for free, with no special-case guard needed.
	branch, err := gitcmd.DefaultBranch(dir, remote)
	if err != nil {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpPush, Status: activity.StatusWarn,
			Msg: "could not resolve default branch on " + remote + ": " + firstLine(err.Error()),
		})
		return 0
	}

	// A repo with only feature branches and no local default branch is
	// skipped, never auto-created.
	if !gitcmd.HasLocalBranch(dir, branch) {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpPush, Status: activity.StatusSkip,
			Branch: branch, Msg: "no local " + branch + " branch, skipping",
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
// Goes through sshx so a peer that needs a password works here too: if one
// is stored, sshx arms the askpass helper instead of BatchMode=yes.
func notify(cfg config.Config, rel, branch string) {
	target := cfg.PeerUser + "@" + cfg.PeerHost
	remote := fmt.Sprintf("~/.gitsync/bin/git-sync receive '%s'", rel)

	cmd := sshx.Command(target, remote)
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
