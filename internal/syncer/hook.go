// Package syncer implements git-sync's three machine-invoked operations:
// the commit hook, the background push, and the peer-side receive.
package syncer

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
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
		// Not installed, or mid-uninstall. Silently do nothing.
		return nil
	}

	root, err := gitcmd.Toplevel(dir)
	if err != nil {
		return nil // not a git repo; nothing to sync
	}

	// git resolves symlinks in --show-toplevel (e.g. macOS's /var ->
	// /private/var), but base_dir as configured is not resolved. Compare
	// resolved forms so a symlinked ancestor doesn't look like "outside
	// base_dir".
	base := cfg.BaseDir
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	rc := cfg
	rc.BaseDir = base

	rel, err := rc.RepoRel(root)
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

	return spawn(rel)
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
