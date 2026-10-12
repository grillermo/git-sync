package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/grillermo/git-sync/internal/config"
)

// UpdateMesh replaces the installed git-sync on every machine in the mesh
// with self (or the build for each peer's platform beside it), this machine
// included. Nothing needs restarting by hand: the binary is renamed into
// place, so the watch service sees a new file and exits to be restarted on
// it, and hooks and receives pick it up on their next run. An unreachable
// peer is a warning, finished by running the same command again.
func UpdateMesh(cfg config.Config, self string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if sameFile(self, config.BinPath()) {
		// Not an error - it still repairs a peer left on an older copy - but
		// almost never what was meant after a rebuild.
		fmt.Fprintln(out, "NOTE: this is the installed copy, not a new build; "+
			"to send a rebuild, run the repo's bin/git-sync update-mesh")
	} else {
		if err := os.MkdirAll(filepath.Dir(config.BinPath()), 0o755); err != nil {
			return err
		}
		if err := copyExecutable(self, config.BinPath()); err != nil {
			return fmt.Errorf("updating the installed binary: %w", err)
		}
		fmt.Fprintln(out, "updated this machine")
	}

	var failed []string
	for _, p := range cfg.PeerList() {
		_, err := sendPeerBinary(p, self)
		switch {
		case err == nil:
			fmt.Fprintf(out, "updated %s\n", p.Host)
		case IsPeerUnreachable(err):
			fmt.Fprintf(out, "WARNING: %s unreachable; run this again once it is up\n", p.Host)
		default:
			fmt.Fprintf(out, "WARNING: %s: %v\n", p.Host, err)
			failed = append(failed, p.Host)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed on %d of %d peer(s): %s", len(failed), len(cfg.PeerList()), strings.Join(failed, ", "))
	}
	return nil
}
