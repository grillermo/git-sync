package setup

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/grillermo/git-sync/internal/sshx"
)

// Reachable checks that this machine can ssh to target without a prompt.
// git-sync is key-only: nothing in the sync path has a terminal to answer a
// password prompt with, so a peer that wants one is not usable. Reach wraps
// this for install, which turns the error into a specific fix.
func Reachable(target string) error {
	out, err := sshx.Command(target, "true").CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	_ = errors.As(err, &ee)
	// The whole of ssh's output, not just its first line: a changed host key
	// opens with a banner of @s, and FixesFor needs the words after it.
	return fmt.Errorf("%w: %s: %s", errPeerUnreachable, target, strings.TrimSpace(string(out)))
}
