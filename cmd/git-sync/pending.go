package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// pendingRepos is the repo selection from an install that has not finished
// pairing the mesh yet. Getting every machine to ssh to every other can take
// several runs of install; re-ticking the same forty repos on each one would
// be the worst part of that. It lives in the temp dir, keyed by base_dir, and
// is removed once a run pairs every machine.
type pendingRepos struct {
	BaseDir string   `json:"base_dir"`
	Repos   []string `json:"repos"`
}

// pendingPath is per-user: /tmp is shared on Linux.
func pendingPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("git-sync-install-repos-%d.json", os.Getuid()))
}

// loadPending returns the saved selection for base, or nil if there is none,
// it is for a different base_dir, or it cannot be read.
func loadPending(base string) []string {
	b, err := os.ReadFile(pendingPath())
	if err != nil {
		return nil
	}
	var p pendingRepos
	if json.Unmarshal(b, &p) != nil || p.BaseDir != base || len(p.Repos) == 0 {
		return nil
	}
	return p.Repos
}

func savePending(base string, repos []string) error {
	b, err := json.Marshal(pendingRepos{BaseDir: base, Repos: repos})
	if err != nil {
		return err
	}
	return os.WriteFile(pendingPath(), b, 0o600)
}

func clearPending() {
	_ = os.Remove(pendingPath())
}
