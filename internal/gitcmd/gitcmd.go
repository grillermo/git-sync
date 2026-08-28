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

// AheadBehind counts the commits branch has that remote/branch does not, and
// the other way round. Both zero means the two are the same commit; both
// non-zero means they have diverged and only a merge can reconcile them.
//
// Reads the remote-tracking ref, so it reports whatever the last fetch saw -
// call Fetch first for an answer about the remote as it is now.
func AheadBehind(dir, remote, branch string) (ahead, behind int, err error) {
	out, err := Run(dir, "rev-list", "--left-right", "--count",
		remote+"/"+branch+"..."+branch)
	if err != nil {
		return 0, 0, err
	}
	// git prints "<left>\t<right>": left is remote-only (behind), right is
	// local-only (ahead).
	if _, err := fmt.Sscan(out, &behind, &ahead); err != nil {
		return 0, 0, fmt.Errorf("unreadable rev-list count %q: %w", out, err)
	}
	return ahead, behind, nil
}

// RemoteURL is what the two machines must agree on: same remote name is not
// enough if they point at different repositories.
func RemoteURL(dir, remote string) (string, error) {
	return Run(dir, "remote", "get-url", remote)
}

var errNoDefaultBranch = errors.New("no default branch")

// IsNoDefaultBranch reports whether err means the remote's default branch
// could not be resolved. Distinct from IsNoRemote: the remote is fine, its
// HEAD just isn't known and couldn't be fetched.
func IsNoDefaultBranch(err error) bool { return errors.Is(err, errNoDefaultBranch) }

// DefaultBranch returns the remote's default branch (bare, e.g. "main"),
// resolved once and cached in refs/remotes/<remote>/HEAD. This is where the
// two machines meet, independent of what is checked out on either side.
func DefaultBranch(dir, remote string) (string, error) {
	if b, err := headRef(dir, remote); err == nil {
		return b, nil
	}
	// refs/remotes/<remote>/HEAD is set by clone and remote rename but not by
	// a bare `remote add`; fetch so the remote's branches exist locally, then
	// ask the remote which one is default and cache it.
	_, _ = Run(dir, "fetch", remote)
	_, _ = Run(dir, "remote", "set-head", remote, "-a")
	b, err := headRef(dir, remote)
	if err != nil {
		return "", fmt.Errorf("%w: %s on %s", errNoDefaultBranch, remote, dir)
	}
	return b, nil
}

func headRef(dir, remote string) (string, error) {
	out, err := Run(dir, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(out, remote+"/"), nil
}

// HasLocalBranch reports whether the repo has a local branch of this name.
// A repo with only feature branches and no local default branch is skipped,
// never auto-created.
func HasLocalBranch(dir, branch string) bool {
	_, err := Run(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

var errNotFastForward = errors.New("not a fast-forward")

// IsNotFastForward reports whether err means FastForwardRef refused to move
// the ref because local history is not an ancestor of the remote's.
func IsNotFastForward(err error) bool { return errors.Is(err, errNotFastForward) }

// FastForwardRef advances a branch that is NOT checked out to <remote>/<branch>
// without touching the working tree. update-ref refuses nothing on its own, so
// this checks ancestry first with merge-base --is-ancestor (true, and a no-op
// move, when local already equals remote); a plain update-ref here would
// silently drop commits on a diverged branch.
func FastForwardRef(dir, remote, branch string) error {
	if _, err := Run(dir, "merge-base", "--is-ancestor", "refs/heads/"+branch, remote+"/"+branch); err != nil {
		return fmt.Errorf("%w: refs/heads/%s is not an ancestor of %s/%s", errNotFastForward, branch, remote, branch)
	}
	_, err := Run(dir, "update-ref", "refs/heads/"+branch, remote+"/"+branch)
	return err
}

// Merge performs a real (non-ff) merge of <remote>/<branch> into HEAD. Used
// only by install-time convergence, never by receive. Returns an error on
// conflicts; the caller then MergeAbort()s.
func Merge(dir, remote, branch string) error {
	_, err := Run(dir, "merge", "--no-edit", remote+"/"+branch)
	return err
}

// MergeAbort aborts an in-progress merge, restoring HEAD and the working tree
// to their pre-merge state. Call it even when Merge only partially applied
// (e.g. some conflicts were resolved already) so a stashed or mid-merge tree
// is never left half-done.
func MergeAbort(dir string) error {
	_, err := Run(dir, "merge", "--abort")
	return err
}

func Stash(dir string) error {
	_, err := Run(dir, "stash", "push", "-u", "-m", "git-sync auto-stash")
	return err
}

func StashPop(dir string) error {
	_, err := Run(dir, "stash", "pop")
	return err
}
