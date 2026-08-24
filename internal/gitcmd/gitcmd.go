// Package gitcmd is a thin wrapper over the git binary. git-sync shells out
// rather than linking a git library: the whole point is to behave exactly
// like the git the user already has, including their config and credentials.
package gitcmd

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/grillermo/git-sync/internal/activity"
)

// Run executes git in dir and returns its trimmed stdout. Raw output goes to
// the debug log so failures can be diagnosed after the fact.
func Run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		activity.AppendDebug(fmt.Sprintf("git %s (in %s) failed: %v\n%s",
			strings.Join(args, " "), dir, err, text))
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// Toplevel returns the root of the repo containing dir.
func Toplevel(dir string) (string, error) {
	return Run(dir, "rev-parse", "--show-toplevel")
}

var errDetachedHead = errors.New("detached HEAD")

// IsDetachedHead reports whether err came from a repo with no checked-out
// branch. Syncing one is out of scope: there is no branch to name on the
// remote, and the remote is where the two machines meet.
func IsDetachedHead(err error) bool { return errors.Is(err, errDetachedHead) }

func CurrentBranch(dir string) (string, error) {
	out, err := Run(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("%w: %v", errDetachedHead, err)
	}
	return out, nil
}

// IsDirty asks git directly whether the working tree has changes, including
// untracked files. Never infer this from `git pull`'s exit code: that cannot
// distinguish a dirty tree from diverged history.
func IsDirty(dir string) (bool, error) {
	out, err := Run(dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

var errNoRemote = errors.New("no shared remote")

// IsNoRemote reports whether err means the repo has nothing to sync through.
// Both machines meet at a remote; a repo without one can never sync, and
// saying so is more useful than a generic failure.
func IsNoRemote(err error) bool { return errors.Is(err, errNoRemote) }

// Remotes lists the repo's configured remotes.
func Remotes(dir string) ([]string, error) {
	out, err := Run(dir, "remote")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ResolveRemote picks the remote that carries this repo between the machines:
// the first name in prefs that the repo actually has - `github` before
// `origin` by default - and failing that its sole remote, if it has exactly
// one. Several unlisted remotes is an ambiguity we refuse to guess at, because
// pushing to the wrong one syncs nothing while looking like it worked.
//
// Deliberately independent of the branch's @{upstream}: a branch can track
// origin/main in a repo whose shared remote is github, and both machines must
// resolve to the same answer from the same rule.
func ResolveRemote(dir string, prefs []string) (string, error) {
	have, err := Remotes(dir)
	if err != nil {
		return "", err
	}
	set := make(map[string]bool, len(have))
	for _, r := range have {
		set[r] = true
	}
	for _, want := range prefs {
		if set[want] {
			return want, nil
		}
	}
	switch len(have) {
	case 0:
		return "", fmt.Errorf("%w: %s has no remotes", errNoRemote, dir)
	case 1:
		return have[0], nil
	default:
		return "", fmt.Errorf("%w: %s has remotes %s but none named %s",
			errNoRemote, dir, strings.Join(have, ", "), strings.Join(prefs, " or "))
	}
}

// HasRemoteBranch reports whether remote/branch exists locally after a fetch.
// Checked separately because a missing remote-tracking ref would otherwise
// fail the fast-forward merge and be misreported as "diverged".
func HasRemoteBranch(dir, remote, branch string) bool {
	_, err := Run(dir, "rev-parse", "--verify", "--quiet", remote+"/"+branch)
	return err == nil
}

func Fetch(dir, remote string) error {
	_, err := Run(dir, "fetch", remote)
	return err
}

// Push sends branch to the named remote. Explicit rather than a bare
// `git push` so it cannot follow a branch upstream that points somewhere else.
func Push(dir, remote, branch string) (string, error) {
	return Run(dir, "push", remote, branch)
}

// FastForward attempts a fast-forward-only merge of remote/branch. It fails,
// by design, when history has diverged - merging is the user's call.
func FastForward(dir, remote, branch string) error {
	_, err := Run(dir, "merge", "--ff-only", remote+"/"+branch)
	return err
}

// RemoteURL is what the two machines must agree on: same remote name is not
// enough if they point at different repositories.
func RemoteURL(dir, remote string) (string, error) {
	return Run(dir, "remote", "get-url", remote)
}

func Stash(dir string) error {
	_, err := Run(dir, "stash", "push", "-u", "-m", "git-sync auto-stash")
	return err
}

func StashPop(dir string) error {
	_, err := Run(dir, "stash", "pop")
	return err
}
