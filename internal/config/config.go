// Package config holds git-sync's on-disk configuration and the rules that
// map between absolute repo paths and the relative paths that identify a repo
// across both machines.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is ~/.gitsync/config.toml. It is hand-editable by design.
type Config struct {
	// BaseDir is the sync root. Only repos under it sync, and a repo's
	// identity across machines is its path relative to BaseDir.
	BaseDir string `toml:"base_dir"`
	// PeerHost is the other machine, as reachable from this one over ssh.
	PeerHost string `toml:"peer_host"`
	// PeerUser is the account to ssh in as on the peer.
	PeerUser string `toml:"peer_user"`
	// Repos is the allowlist: paths relative to BaseDir, chosen through the
	// install picker. Nothing outside it is ever pushed or received.
	Repos []string `toml:"repos"`
	// RemoteNames is the preference order for the shared remote that actually
	// carries the commits between the machines. Empty means DefaultRemoteNames.
	RemoteNames []string `toml:"remote_names"`
}

// DefaultRemoteNames is the preference order when config.toml says nothing:
// a repo pushed to a remote called `github` uses that, otherwise `origin`.
var DefaultRemoteNames = []string{"github", "origin"}

// Remotes is the preference order to try, never empty.
func (c Config) Remotes() []string {
	if len(c.RemoteNames) == 0 {
		return DefaultRemoteNames
	}
	return c.RemoteNames
}

// Home is git-sync's state directory. GITSYNC_HOME overrides it; tests rely
// on that, and so does anyone running two installs on one machine.
func Home() string {
	if h := os.Getenv("GITSYNC_HOME"); h != "" {
		return h
	}
	return filepath.Join(os.Getenv("HOME"), ".gitsync")
}

func Path() string         { return filepath.Join(Home(), "config.toml") }
func ActivityPath() string { return filepath.Join(Home(), "activity.jsonl") }
func DebugLogPath() string { return filepath.Join(Home(), "debug.log") }
func LocksDir() string     { return filepath.Join(Home(), "locks") }
func BinPath() string      { return filepath.Join(Home(), "bin", "git-sync") }
func HooksDir() string     { return filepath.Join(Home(), "hooks") }
func AskpassPath() string  { return filepath.Join(Home(), "askpass") }

var errNotInstalled = errors.New("git-sync is not installed")

// IsNotInstalled reports whether err means "no config yet", so callers can say
// "run git-sync install" instead of leaking a file-not-found path.
func IsNotInstalled(err error) bool {
	return errors.Is(err, errNotInstalled) || errors.Is(err, fs.ErrNotExist)
}

func Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return c, fmt.Errorf("%w: no config at %s", errNotInstalled, Path())
		}
		return c, err
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parsing %s: %w", Path(), err)
	}
	return c, nil
}

func (c Config) Save() error {
	// Write the effective preference order, not an empty list: the file is
	// meant to be read and edited, and a silent default is invisible there.
	c.RemoteNames = c.Remotes()
	if err := os.MkdirAll(Home(), 0o755); err != nil {
		return err
	}
	f, err := os.Create(Path())
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprint(f, "# git-sync configuration. Edit freely.\n"); err != nil {
		return err
	}
	return toml.NewEncoder(f).Encode(c)
}

// RepoRel converts an absolute repo path into the relative path that
// identifies it on both machines. It fails for anything not strictly under
// BaseDir — such a repo has no cross-machine identity we can act on.
func (c Config) RepoRel(abs string) (string, error) {
	base := filepath.Clean(c.BaseDir)
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return "", err
	}
	// Rel happily returns "../x" and "."; neither is under base.
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is not under base_dir %s", abs, base)
	}
	return filepath.ToSlash(rel), nil
}

// RepoPath is the inverse of RepoRel. Callers must ValidateRel first.
func (c Config) RepoPath(rel string) string {
	return filepath.Join(c.BaseDir, filepath.FromSlash(rel))
}

// IsSelected reports whether rel is in the allowlist. Called on every commit
// via the hook, and on every incoming sync, so it is the single gate that
// decides whether git-sync touches a repo at all.
func (c Config) IsSelected(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	for _, r := range c.Repos {
		if filepath.ToSlash(filepath.Clean(filepath.FromSlash(r))) == rel {
			return true
		}
	}
	return false
}

// ValidateRel rejects a relative path that would escape BaseDir. The relpath
// arrives from the peer over ssh, so it is untrusted input.
func (c Config) ValidateRel(rel string) error {
	if rel == "" {
		return errors.New("empty repo path")
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("repo path %q must be relative", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("repo path %q escapes base_dir", rel)
	}
	if clean == "." {
		return fmt.Errorf("repo path %q must not be the base_dir itself", rel)
	}
	return nil
}
