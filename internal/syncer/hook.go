// Package syncer implements git-sync's three machine-invoked operations:
// the commit hook, the background push, and the peer-side receive.
package syncer

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
	"github.com/grillermo/git-sync/internal/lock"
)

// Spawner starts a background push for a repo. Injected so tests can observe
// the decision without forking a process.
type Spawner func(rel string) error

// Hook runs on every commit, in every repo on the machine. git waits on this
// process, so it must do almost nothing: identify the repo and hand off.
//
// It returns an error only for genuinely broken state. Anything expected -
// no config, a repo outside base_dir - is logged and swallowed, because a
// failing post-commit hook must never break the user's commit.
func Hook(dir string, spawn Spawner) error {
	cfg, err := config.Load()
	if err != nil {
		if !config.IsNotInstalled(err) {
			// A genuinely unexpected failure - permission error, disk issue,
			// or a corrupt hand-edited config.toml - not the ordinary "not
			// installed" case. Trace it so it's not invisible, but still
			// never break the commit.
			activity.AppendDebug(fmt.Sprintf("hook: config.Load: %v", err))
		}
		return nil
	}

	root, err := gitcmd.Toplevel(dir)
	if err != nil {
		return nil // not a git repo; nothing to sync
	}

	rel, err := repoRel(cfg, root)
	if err != nil {
		_ = activity.Append(activity.Event{
			Repo: root, Op: activity.OpHook, Status: activity.StatusSkip,
			Msg: "outside base_dir, not synced",
		})
		return nil
	}

	// Sync is opt-in. The hook fires in every repo on the machine, so an
	// unselected repo is the common case, not an event - recording it would
	// bury the real activity under noise.
	if !cfg.IsSelected(rel) {
		return nil
	}

	// A commit made while this machine is applying someone else's changes
	// must not broadcast: for this repo we are a receiver, not a
	// broadcaster. --no-verify skips pre-commit but not post-commit, which
	// is exactly the case this catches.
	if owner, held := lock.Held(rel); held {
		_ = activity.Append(activity.Event{
			Repo: rel, Op: activity.OpHook, Status: activity.StatusWarn,
			Peer: owner.From,
			Msg:  "committed while receiving from " + owner.From + ", not broadcast",
		})
		return nil
	}

	return spawn(rel)
}

// repoRel resolves root's relpath under cfg's base_dir. Shared by Hook and
// selectedRel so the symlink-resolution rule below lives in exactly one
// place.
//
// git resolves symlinks in --show-toplevel (e.g. macOS's /var ->
// /private/var), but base_dir as configured is not resolved. Compare
// resolved forms so a symlinked ancestor doesn't look like "outside
// base_dir".
func repoRel(cfg config.Config, root string) (string, error) {
	base := cfg.BaseDir
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	rc := cfg
	rc.BaseDir = base
	return rc.RepoRel(root)
}

// Block is the body of the pre-commit and pre-push hooks: it refuses to let
// the user commit or push in a repo this machine is currently receiving.
//
// It fails open in every other case. git-sync being broken - no config, an
// unreadable one, a repo outside base_dir - must never stop someone
// committing; only a lock that is genuinely held blocks.
func Block(dir string, w io.Writer) int {
	if os.Getenv("GITSYNC_INTERNAL") != "" {
		return 0 // git-sync's own push, not the user's
	}
	rel, ok := selectedRel(dir)
	if !ok {
		return 0
	}
	owner, held := lock.Held(rel)
	if !held {
		return 0
	}
	from := owner.From
	if from == "" {
		from = "another machine"
	}
	fmt.Fprintf(w, "git-sync: %s is receiving changes from %s (started %s ago).\n",
		rel, from, owner.Age().Round(time.Second))
	fmt.Fprintf(w, "Wait a moment and try again. If this is stuck: git-sync unlock %s\n", rel)
	return 1
}

// selectedRel is the repo-identification half of Hook: the relpath of the
// repo containing dir, and whether it is one git-sync syncs. Every error is
// "not ours", because both callers must carry on regardless.
func selectedRel(dir string) (string, bool) {
	cfg, err := config.Load()
	if err != nil {
		return "", false
	}
	root, err := gitcmd.Toplevel(dir)
	if err != nil {
		return "", false
	}
	rel, err := repoRel(cfg, root)
	if err != nil {
		return "", false
	}
	return rel, cfg.IsSelected(rel)
}

// SpawnDetached starts `self push <rel>` in its own session and returns at
// once, without waiting for it.
//
// Detaching matters twice over: git waits for the hook to exit, and git also
// reads the hook's stdout. A child holding that inherited pipe open would
// stall the commit for as long as the push took - which is the exact problem
// this design exists to avoid. So the child gets its own session and its
// stdio pointed at the debug log.
func SpawnDetached(self, rel string) error {
	logf, err := os.OpenFile(config.DebugLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logf = nil
	}

	cmd := exec.Command(self, "push", rel)
	cmd.Stdin = nil
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
		defer logf.Close()
	}
	cmd.SysProcAttr = detachAttr()

	if err := cmd.Start(); err != nil {
		return err
	}
	// Release, never Wait: we are not the child's keeper.
	return cmd.Process.Release()
}
