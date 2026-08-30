package setup

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/gitcmd"
)

// Cloning the missing half of a pair.
//
// A repo the user selected here but never cloned on the peer is one of the two
// failures repocheck.go exists to catch, and until now the only answer was a
// sentence telling the user to go clone it by hand. But everything needed to do
// it for them is already known at install time: the relative path (the peer's
// base_dir plus the same rel), and the remote URL this machine syncs through -
// which is the whole transport, so a clone from it lands the peer on exactly
// the repository the two machines will meet at.
//
// The order matters. This machine pushes its default branch to the shared
// remote *first*, then the peer clones: a clone can only contain what the
// remote already has, so cloning first would hand the peer a copy missing the
// local commits and leave it behind from the moment it existed.
//
// Only a genuinely missing path is ever cloned. A directory that exists but is
// not a repo, a clone of a different remote, or a repo we could not ask about
// is still reported for the user - writing into any of those would be
// destroying something we do not understand.

// cloneMarker opens the remote script, so it is identifiable in the debug log
// and in the ssh stub's recorded calls.
const cloneMarker = "# git-sync-clone-missing"

// Clonable reports whether this check is one install can fix by cloning: the
// path is empty on the peer, and we know a remote URL to clone from. The quote
// tests mirror every other remote command in this package - the rel and the URL
// are interpolated into a single-quoted shell string, and a value that would
// break out of the quotes is reported rather than sent.
func (c RepoCheck) Clonable() bool {
	return c.State == RepoMissing && c.RemoteURL != "" &&
		!strings.Contains(c.Rel, "'") && !strings.Contains(c.RemoteURL, "'")
}

// ClonableChecks picks out the repos CloneMissing would act on, in order.
func ClonableChecks(checks []RepoCheck) []RepoCheck {
	var out []RepoCheck
	for _, c := range checks {
		if c.Clonable() {
			out = append(out, c)
		}
	}
	return out
}

// CloneResult is what happened to one repo. Cloned and PushNote are
// independent: a repo whose push failed can still clone (the remote had
// everything it needed already), and saying only "cloned" would hide the
// commits that did not make it there.
type CloneResult struct {
	Rel       string
	RemoteURL string
	Cloned    bool
	PushNote  string // why this machine could not publish first, if it could not
	Err       string // why the peer does not have it, if it still does not
}

// CloneMissing publishes each repo to its shared remote and then clones it on
// the peer, in one ssh round trip for all of them. Never fatal: a repo that
// cannot be cloned is reported and the install carries on, exactly as an
// unclonable mismatch always has been.
func CloneMissing(target, peerBase string, cfg config.Config, todo []RepoCheck) []CloneResult {
	results := make([]CloneResult, 0, len(todo))
	type job struct{ rel, url, origin string }
	var jobs []job

	for _, c := range todo {
		if !c.Clonable() {
			continue // never trust the caller's filtering with a shell command
		}
		res := CloneResult{Rel: c.Rel, RemoteURL: c.RemoteURL}
		origin, note := publishHere(cfg, c.Rel)
		res.PushNote = note
		if origin == "" {
			// No remote resolvable here at all, so there is no name to give the
			// peer's remote and no reason to believe the URL is current.
			res.Err = note
			results = append(results, res)
			continue
		}
		results = append(results, res)
		jobs = append(jobs, job{rel: c.Rel, url: c.RemoteURL, origin: origin})
	}
	if len(jobs) == 0 {
		return results
	}
	if strings.Contains(peerBase, "'") {
		for i := range results {
			if results[i].Err == "" {
				results[i].Err = "the peer's base_dir is not usable over ssh"
			}
		}
		return results
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\nbase='%s'\n%s", cloneMarker, peerBase, cloneFunc)
	for _, j := range jobs {
		fmt.Fprintf(&b, "clone_one '%s' '%s' '%s'\n", j.rel, j.url, j.origin)
	}
	out, err := sshOut(target, b.String())
	if err != nil {
		for i := range results {
			if results[i].Err == "" {
				results[i].Err = "could not reach " + target
			}
		}
		return results
	}

	// Fold the answers in by name, so a dropped or reordered line leaves that
	// one repo unexplained rather than crediting another repo's outcome to it.
	done := parseCloneOutput(out)
	for i := range results {
		if results[i].Err != "" {
			continue
		}
		msg, answered := done[results[i].Rel]
		switch {
		case !answered:
			results[i].Err = "the peer did not report on this repo"
		case msg == "":
			results[i].Cloned = true
		default:
			results[i].Err = msg
		}
	}
	return results
}

// publishHere pushes rel's default branch to its shared remote, so the peer's
// clone contains this machine's commits rather than starting a step behind. It
// returns the resolved remote's *name*, which the peer's clone reuses via
// --origin so both machines resolve the same remote by the same name.
//
// A push failure is reported but not fatal: the remote may already hold
// everything (nothing to push), or the branch may have diverged - in which case
// the clone is still the right move, and the initial sync that runs next is
// what deals with the divergence.
func publishHere(cfg config.Config, rel string) (origin, note string) {
	dir := cfg.RepoPath(rel)
	remote, err := gitcmd.ResolveRemote(dir, cfg.Remotes())
	if err != nil {
		return "", "no remote to sync through"
	}
	if strings.ContainsAny(remote, "' \t") {
		return "", fmt.Sprintf("remote name %q is not usable over ssh", remote)
	}
	branch, err := gitcmd.DefaultBranch(dir, remote)
	if err != nil {
		return remote, "could not resolve the default branch on " + remote
	}
	if !gitcmd.HasLocalBranch(dir, branch) {
		// Nothing to publish, and nothing to create: a repo with only feature
		// branches is skipped here exactly as it is everywhere else.
		return remote, "no local " + branch + " branch to push first"
	}
	if _, err := gitcmd.Push(dir, remote, branch); err != nil {
		return remote, "could not push " + branch + " to " + remote + " first: " + firstLine(err.Error())
	}
	return remote, ""
}

// parseCloneOutput reads the remote script's answers into rel -> failure
// message, where the empty string means it was cloned.
func parseCloneOutput(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(strings.TrimSpace(sc.Text()))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "ok":
			out[f[1]] = ""
		case "exists":
			// Between the check and now something appeared at that path. Not
			// ours to overwrite, and not something to claim we cloned.
			out[f[1]] = "something already exists at that path on the peer"
		case "fail":
			msg := strings.Join(f[2:], " ")
			if msg == "" {
				msg = "the clone failed on the peer"
			}
			out[f[1]] = msg
		}
	}
	return out
}

// cloneFunc is the peer-side worker. It refuses any path that already exists,
// so this can never write over a directory the check did not classify as
// missing, and it reports git's own first line of failure rather than a
// generic "it did not work".
const cloneFunc = `clone_one() {
  rel="$1"; url="$2"; origin="$3"
  d="$base/$rel"
  if [ -e "$d" ]; then echo "exists $rel"; return; fi
  if ! mkdir -p "$(dirname "$d")" 2>/dev/null; then
    echo "fail $rel could not create the parent directory"; return
  fi
  if err=$(git clone -q --origin "$origin" "$url" "$d" 2>&1); then
    echo "ok $rel"
  else
    echo "fail $rel $(printf '%s' "$err" | tr '\n\t' '  ' | cut -c1-160)"
  fi
}
`

// RenderClonePlan says which repos are about to be cloned on the peer and
// returns whether there are any. Printed before acting: this writes new
// directories on the other machine, which is not something to do silently.
func RenderClonePlan(w io.Writer, peerHost, peerBase string, todo []RepoCheck) bool {
	if len(todo) == 0 {
		return false
	}
	fmt.Fprintf(w, "%d selected repos are missing on %s and will be cloned into %s:\n",
		len(todo), peerHost, peerBase)
	for _, c := range todo {
		fmt.Fprintf(w, "  %-24s %s\n", c.Rel, c.RemoteURL)
	}
	return true
}

// RenderCloneResult reports what happened and returns how many repos the peer
// still does not have. It always prints something: silence would read as "it
// did not try".
func RenderCloneResult(w io.Writer, peerHost string, results []CloneResult) int {
	var cloned, failed []CloneResult
	for _, r := range results {
		if r.Cloned {
			cloned = append(cloned, r)
		} else {
			failed = append(failed, r)
		}
	}
	if len(cloned) > 0 {
		fmt.Fprintf(w, "cloned %d repos on %s\n", len(cloned), peerHost)
	}
	// Notes outlive the clone: a repo can be cloned and still be missing the
	// commits this machine could not push into the remote first.
	for _, r := range cloned {
		if r.PushNote != "" {
			fmt.Fprintf(w, "  %-24s cloned, but %s\n", r.Rel, r.PushNote)
		}
	}
	if len(failed) == 0 {
		if len(cloned) == 0 {
			fmt.Fprintf(w, "nothing to clone on %s\n", peerHost)
		}
		return 0
	}
	fmt.Fprintf(w, "%d repos could not be cloned on %s:\n", len(failed), peerHost)
	for _, r := range failed {
		fmt.Fprintf(w, "  %-24s %s\n", r.Rel, r.Err)
	}
	fmt.Fprintf(w, "These will not sync until you clone them on %s by hand; "+
		"then they sync on the next commit.\n", peerHost)
	return len(failed)
}
