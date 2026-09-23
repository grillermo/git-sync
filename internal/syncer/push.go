package syncer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

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

	notifyAll(cfg, rel, branch)
	return 0
}

// notifyAll tells every other machine to pull what we just pushed. The peers
// are independent: one unreachable machine must not delay or affect the
// others, so they go out concurrently and each gets its own event.
func notifyAll(cfg config.Config, rel, branch string) {
	var wg sync.WaitGroup
	for _, p := range cfg.PeerList() {
		wg.Add(1)
		go func(p config.Peer) {
			defer wg.Done()
			notifyPeer(cfg, p, rel, branch)
		}(p)
	}
	wg.Wait()
}

// notifyPeer asks one peer to run its own receive for this repo.
func notifyPeer(cfg config.Config, p config.Peer, rel, branch string) {
	self, _ := os.Hostname()
	remote := fmt.Sprintf("~/.gitsync/bin/git-sync receive '%s' --from '%s'", rel, sanitizeHost(self))

	cmd := sshx.Command(p.Target(), remote)
	out, err := cmd.CombinedOutput()

	ev := activity.Event{Repo: rel, Op: activity.OpNotify, Branch: branch, Peer: p.Host}
	switch code := exitCode(err); {
	case err == nil:
		ev.Status, ev.Msg = activity.StatusOK, "peer "+p.Host+" synced"
	case code == ExitRepoNotHere:
		// Expected and harmless: the peer has never cloned this repo. No
		// auto-clone, no retry - just say so plainly rather than claiming a
		// sync that never happened.
		ev.Status, ev.Msg = activity.StatusSkip, "peer "+p.Host+" has no copy of this repo, nothing to sync"
	case code == 255:
		ev.Status, ev.Msg = activity.StatusError, "peer "+p.Host+" unreachable"
	default:
		ev.Status = activity.StatusError
		ev.Msg = fmt.Sprintf("peer %s receive failed (exit %d)", p.Host, code)
	}
	// activity.Append is safe to call concurrently without a lock: the log
	// is append-only and each line is kept under PIPE_BUF, so concurrent
	// O_APPEND writes from these goroutines never interleave.
	_ = activity.Append(ev)
	if err != nil {
		activity.AppendDebug("ssh " + p.Target() + ": " + strings.TrimSpace(string(out)))
	}
}

// sanitizeHost reduces a hostname to the characters that are safe both in a
// single-quoted remote command and in a message printed to a terminal.
func sanitizeHost(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_':
			return r
		}
		return -1
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
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
