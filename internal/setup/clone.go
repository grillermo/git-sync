package setup

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// CloneResult is what happened to one repo install tried to clone on a peer.
// Err is empty on success; Branch is the branch the new clone ended up on.
type CloneResult struct {
	Rel    string
	Branch string
	Err    string
}

// cloneMarker opens the remote script, so it is identifiable in the debug log
// and in the ssh stub's recorded calls.
const cloneMarker = "# git-sync-clone"

// Clonable reports whether install can clone r on a peer that is missing it:
// it needs a URL to clone from and a remote name to clone it under. A repo
// with neither was already reported as having nothing to sync through.
func Clonable(r RepoWant) bool { return r.RemoteURL != "" && r.Remote != "" }

// ClonePeerRepos clones each repo onto the peer at the same relative path
// under peerBase, from the URL this machine syncs it through, in one round
// trip. The clone's remote gets this machine's remote name, so push and
// receive on both machines resolve the same remote; and it checks out this
// machine's branch when the remote has it, so the pair is not left on
// different branches.
//
// It never touches a path that already exists: only a repo the check found
// missing is ever cloned, and a directory that appeared since is the user's.
// Nothing on the peer can prompt - there is no terminal behind this ssh - so
// a clone that needs credentials the peer does not have fails and is reported
// instead of hanging the install. Order matches repos, and every entry gets
// exactly one CloneResult.
func ClonePeerRepos(target, peerBase string, repos []RepoWant) ([]CloneResult, error) {
	results := make([]CloneResult, len(repos))
	for i, r := range repos {
		results[i] = CloneResult{Rel: r.Rel, Err: "no answer from the peer"}
	}
	if len(repos) == 0 {
		return results, nil
	}

	out, err := sshOut(target, cloneScript(peerBase, repos))
	if err != nil {
		for i := range results {
			results[i].Err = "could not reach the peer"
		}
		return results, fmt.Errorf("%w: %s: %v", errPeerUnreachable, target, err)
	}

	// Folded in by name, like CheckPeerRepos: a missing line leaves that repo
	// reported as unanswered rather than shifting answers onto the wrong one.
	byRel := map[string]CloneResult{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.SplitN(strings.TrimSpace(sc.Text()), " ", 3)
		if len(f) < 3 {
			continue
		}
		switch f[0] {
		case "cloned":
			byRel[f[1]] = CloneResult{Rel: f[1], Branch: f[2]}
		case "err":
			byRel[f[1]] = CloneResult{Rel: f[1], Err: f[2]}
		}
	}
	for i := range results {
		if r, ok := byRel[results[i].Rel]; ok {
			results[i] = r
		}
	}
	return results, nil
}

// cloneScript builds the remote shell. GIT_TERMINAL_PROMPT=0 and a /dev/null
// stdin make a clone that wants a password fail at once rather than wait on
// a prompt nobody can see.
func cloneScript(peerBase string, repos []RepoWant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nbase=%s\nexport GIT_TERMINAL_PROMPT=0\n", cloneMarker, shellQuote(peerBase))
	b.WriteString(`clone() {
  rel=$1; remote=$2; url=$3; want=$4
  d="$base/$rel"
  if [ -e "$d" ]; then echo "err $rel $d already exists"; return; fi
  mkdir -p "$(dirname "$d")" || { echo "err $rel could not create $(dirname "$d")"; return; }
  if ! msg=$(git clone -q -o "$remote" "$url" "$d" </dev/null 2>&1); then
    echo "err $rel clone from $url failed: $(printf '%s' "$msg" | tail -n 1)"; return
  fi
  if [ -n "$want" ] && git -C "$d" rev-parse --verify --quiet "refs/remotes/$remote/$want" >/dev/null; then
    git -C "$d" checkout -q "$want" >/dev/null 2>&1
  fi
  echo "cloned $rel $(git -C "$d" symbolic-ref --short HEAD 2>/dev/null || echo '(detached)')"
}
`)
	for _, r := range repos {
		fmt.Fprintf(&b, "clone %s %s %s %s\n",
			shellQuote(r.Rel), shellQuote(r.Remote), shellQuote(r.RemoteURL), shellQuote(r.Branch))
	}
	return b.String()
}

// RenderCloneResults prints what was cloned on peerHost and what could not
// be, and returns how many failed.
func RenderCloneResults(w io.Writer, peerHost string, results []CloneResult) int {
	failed := 0
	for _, r := range results {
		if r.Err == "" {
			fmt.Fprintf(w, "  cloned        %s on %s (%s)\n", r.Rel, peerHost, r.Branch)
			continue
		}
		failed++
		fmt.Fprintf(w, "  not cloned    %s on %s: %s\n", r.Rel, peerHost, r.Err)
	}
	if failed > 0 {
		fmt.Fprintf(w, "Clone those on %s by hand at the same relative path, from the same "+
			"remote; they sync on the next commit after that.\n", peerHost)
	}
	return failed
}
