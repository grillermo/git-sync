package setup

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
)

// RepoState is what the peer has at a selected repo's path.
type RepoState string

const (
	RepoPresent     RepoState = "present"      // same repo, same remote; it will sync
	RepoMissing     RepoState = "missing"      // nothing there; it will never sync
	RepoNotAGitRepo RepoState = "not-a-repo"   // a directory, but not a clone
	RepoNoRemote    RepoState = "no-remote"    // cloned, but nothing to sync through
	RepoOtherRemote RepoState = "other-remote" // a clone of a different repository
	RepoUnchecked   RepoState = "unchecked"    // we could not ask about this one
)

// RepoWant is one selected repo as this machine sees it: the path, and the
// remote URL we sync it through. The peer has to match both.
type RepoWant struct {
	Rel       string
	RemoteURL string
}

// RepoCheck is the peer's answer for one selected repo.
type RepoCheck struct {
	Rel           string
	State         RepoState
	RemoteURL     string // ours
	PeerRemoteURL string // theirs, when they have one
}

// repoCheckMarker opens the remote script. It makes the command identifiable
// in the debug log and in the ssh stub's recorded calls.
const repoCheckMarker = "# git-sync-repo-check"

// CheckPeerRepos asks the peer, in one round trip, which of the chosen repos
// it has and which remote each one points at. Two failures hide here, and
// both are silent at runtime: a repo that exists on one machine only never
// syncs, and a repo cloned from a *different* remote on each machine syncs
// forever without the two ever converging - they are pushing into different
// repositories. Order matches repos, and every entry gets exactly one
// RepoCheck. Uses config.DefaultRemoteNames; see CheckPeerReposWithRemotes.
func CheckPeerRepos(target, peerBase string, repos []RepoWant) ([]RepoCheck, error) {
	return CheckPeerReposWithRemotes(target, peerBase, repos, config.DefaultRemoteNames)
}

// CheckPeerReposWithRemotes is CheckPeerRepos with an explicit remote-name
// preference order, so the peer resolves the remote by the same rule push and
// receive will.
func CheckPeerReposWithRemotes(target, peerBase string, repos []RepoWant, remotePrefs []string) ([]RepoCheck, error) {
	checks := make([]RepoCheck, len(repos))
	var askable []string
	for i, r := range repos {
		checks[i] = RepoCheck{Rel: r.Rel, State: RepoUnchecked, RemoteURL: r.RemoteURL}
		if !strings.Contains(r.Rel, "'") {
			askable = append(askable, r.Rel)
		}
	}
	if len(askable) == 0 || strings.Contains(peerBase, "'") {
		return checks, nil
	}
	for _, name := range remotePrefs {
		if strings.Contains(name, "'") {
			return checks, fmt.Errorf("remote name %q is not usable over ssh", name)
		}
	}

	out, err := sshOut(target, repoCheckScript(peerBase, askable, remotePrefs))
	if err != nil {
		return checks, fmt.Errorf("%w: %s: %v", errPeerUnreachable, target, err)
	}

	// Fold the answers in by name, so a missing or reordered line leaves that
	// repo unchecked rather than shifting every later answer onto the wrong one.
	type answer struct {
		state RepoState
		url   string
	}
	byRel := map[string]answer{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		state, rest, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		switch RepoState(state) {
		case "url":
			rel, url, hasURL := strings.Cut(rest, " ")
			if !hasURL {
				continue
			}
			byRel[rel] = answer{url: url}
		case RepoMissing, RepoNotAGitRepo, RepoNoRemote:
			byRel[rest] = answer{state: RepoState(state)}
		}
	}

	for i := range checks {
		a, ok := byRel[checks[i].Rel]
		if !ok {
			continue // stays unchecked
		}
		if a.state != "" {
			checks[i].State = a.state
			continue
		}
		checks[i].PeerRemoteURL = a.url
		if sameRemote(checks[i].RemoteURL, a.url) {
			checks[i].State = RepoPresent
		} else {
			checks[i].State = RepoOtherRemote
		}
	}
	return checks, nil
}

// sameRemote compares two remote URLs the way a person would: a trailing
// `.git` or `/` is punctuation, not a different repository. Anything subtler
// (ssh vs https for the same host and path) is left alone - reporting a
// difference the user can dismiss beats hiding one that matters.
func sameRemote(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "/")
		return strings.TrimSuffix(s, ".git")
	}
	return norm(a) == norm(b)
}

// repoCheckScript builds the remote shell. `.git` is tested with -e, not -d:
// in a worktree or a submodule it is a file. The remote is resolved by the
// same preference order this machine uses, then its sole remote as a
// fallback - the shell mirror of gitcmd.ResolveRemote.
func repoCheckScript(peerBase string, repos, remotePrefs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nbase='%s'\nprefs=\"", repoCheckMarker, peerBase)
	b.WriteString(strings.Join(remotePrefs, " "))
	b.WriteString("\"\nfor rel in")
	for _, rel := range repos {
		fmt.Fprintf(&b, " '%s'", rel)
	}
	b.WriteString(`; do
  d="$base/$rel"
  if [ ! -e "$d" ]; then echo "missing $rel"; continue; fi
  if [ ! -e "$d/.git" ]; then echo "not-a-repo $rel"; continue; fi
  u=""
  for r in $prefs; do
    u=$(git -C "$d" remote get-url "$r" 2>/dev/null) && break
    u=""
  done
  if [ -z "$u" ] && [ "$(git -C "$d" remote | wc -l | tr -d ' ')" = "1" ]; then
    u=$(git -C "$d" remote get-url "$(git -C "$d" remote)" 2>/dev/null)
  fi
  if [ -z "$u" ]; then echo "no-remote $rel"; else echo "url $rel $u"; fi
done
`)
	return b.String()
}

// RenderRepoChecks prints the mismatches and returns how many there were.
// It always prints something: silence would read as "it did not look".
func RenderRepoChecks(w io.Writer, peerHost, peerBase string, checks []RepoCheck) int {
	var bad []RepoCheck
	for _, c := range checks {
		if c.State != RepoPresent {
			bad = append(bad, c)
		}
	}
	if len(bad) == 0 {
		fmt.Fprintf(w, "all %d selected repos are present on %s\n", len(checks), peerHost)
		return 0
	}

	fmt.Fprintf(w, "%d of %d selected repos are not ready on %s (%s):\n",
		len(bad), len(checks), peerHost, peerBase)
	for _, c := range bad {
		fmt.Fprintf(w, "  %-13s %s\n", c.State, c.Rel)
		if c.State == RepoOtherRemote {
			// Both URLs, because the whole point is that they differ.
			fmt.Fprintf(w, "                here: %s\n", c.RemoteURL)
			fmt.Fprintf(w, "                %-4s: %s\n", "peer", c.PeerRemoteURL)
		}
	}
	fmt.Fprintf(w, "These will not sync until you clone them on %s under %s at "+
		"the same relative path, from the same remote; then they sync on the "+
		"next commit.\n", peerHost, peerBase)
	return len(bad)
}
