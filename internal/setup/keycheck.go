package setup

import (
	"fmt"
	"io"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
)

// KeyResult is one ordered pair: can From ssh to To without a prompt?
type KeyResult struct {
	From config.Peer
	To   config.Peer
	OK   bool
	Err  string
}

// keyCheckMarker opens the remote command, so it is identifiable in the debug
// log and in the ssh stub's recorded calls.
const keyCheckMarker = "# git-sync-key-check"

// CheckKeys probes every ordered pair of machines in the mesh. git-sync is
// key-only and every machine notifies every other, so a missing key between
// two *peers* - a pair this machine never exercises itself - would otherwise
// only show up later as a failing notify nobody is watching.
func CheckKeys(self config.Peer, peers []config.Peer) []KeyResult {
	var out []KeyResult
	for _, p := range peers {
		out = append(out, probeKey(self, p, func(to config.Peer) error {
			return ssh(to.Target(), keyCheckMarker+"\ntrue")
		}))
	}
	for _, from := range peers {
		for _, to := range peers {
			if strings.EqualFold(from.Host, to.Host) {
				continue
			}
			to := to
			out = append(out, probeKey(from, to, func(to config.Peer) error {
				// Run the check *on* `from`, targeting `to`.
				return ssh(from.Target(), fmt.Sprintf(
					"%s\nssh -o BatchMode=yes -o ConnectTimeout=5 %s true",
					keyCheckMarker, to.Target()))
			}))
		}
	}
	return out
}

func probeKey(from, to config.Peer, run func(config.Peer) error) KeyResult {
	r := KeyResult{From: from, To: to, OK: true}
	if err := run(to); err != nil {
		r.OK, r.Err = false, firstLine(err.Error())
	}
	return r
}

// RenderKeyChecks prints the pairs that cannot connect and returns how many
// there were. Install warns on these rather than refusing: the rest of the
// mesh is still worth setting up, and those pairs simply will not sync until
// a key is added.
func RenderKeyChecks(w io.Writer, results []KeyResult) int {
	var bad []KeyResult
	for _, r := range results {
		if !r.OK {
			bad = append(bad, r)
		}
	}
	if len(bad) == 0 {
		return 0
	}
	fmt.Fprintf(w, "\n%d machine pairs cannot ssh to each other:\n", len(bad))
	for _, r := range bad {
		fmt.Fprintf(w, "  %s -> %s: %s\n", r.From.Host, r.To.Host, r.Err)
		fmt.Fprintf(w, "      fix on %s with: ssh-copy-id %s\n", r.From.Host, r.To.Target())
	}
	fmt.Fprintln(w, "  those directions will not sync until a key is in place; the rest will.")
	return len(bad)
}
