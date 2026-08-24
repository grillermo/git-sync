// Package scan discovers the git repos under a base directory, with enough
// metadata for a user to decide whether to sync each one.
package scan

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
)

// Repo is one discovered repository.
type Repo struct {
	Rel        string    // path relative to base_dir; the cross-machine identity
	Abs        string    // absolute path on this machine
	Commits    int       // commits reachable from HEAD; 0 if none yet
	LastCommit time.Time // zero if the repo has no commits
	Remote     string    // the remote it would sync through; "" if it has none
	RemoteURL  string    // that remote's URL; the thing both machines must share
}

// CanSync reports whether syncing this repo could do anything at all. The
// remote is the transport, so a repo without one is worth showing but not
// worth ticking.
func (r Repo) CanSync() bool { return r.Remote != "" }

// Repos walks base and returns every git repo under it, sorted by Rel.
// remotePrefs is the remote-name preference order; nil means the default.
func Repos(base string, remotePrefs []string) ([]Repo, error) {
	if len(remotePrefs) == 0 {
		remotePrefs = config.DefaultRemoteNames
	}
	var found []Repo

	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is not worth failing the whole scan
			// over - skip it and keep going.
			return nil //nolint:nilerr
		}
		if !d.IsDir() || path == base {
			return nil
		}
		// Never descend into dot directories: .cache, .Trash and friends hold
		// checkouts nobody means to sync.
		if strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr != nil {
			return nil
		}

		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return nil
		}
		found = append(found, describe(filepath.ToSlash(rel), path, remotePrefs))
		// A repo's contents cannot hold a separately syncable repo.
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Rel < found[j].Rel })
	return found, nil
}

// describe reads the metadata shown in the picker. A repo with no commits is
// normal (a fresh git init), so failures here are recorded as zero values
// rather than errors.
func describe(rel, abs string, remotePrefs []string) Repo {
	r := Repo{Rel: rel, Abs: abs}
	if remote, err := gitcmd.ResolveRemote(abs, remotePrefs); err == nil {
		r.Remote = remote
		r.RemoteURL, _ = gitcmd.RemoteURL(abs, remote)
	}
	if out, err := gitcmd.Run(abs, "rev-list", "--count", "HEAD"); err == nil {
		r.Commits, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if out, err := gitcmd.Run(abs, "log", "-1", "--format=%ct"); err == nil {
		if secs, convErr := strconv.ParseInt(strings.TrimSpace(out), 10, 64); convErr == nil {
			r.LastCommit = time.Unix(secs, 0)
		}
	}
	return r
}
