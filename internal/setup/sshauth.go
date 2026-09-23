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
// password prompt with, so a peer that wants one is not usable.
func Reachable(target string) error {
	out, err := sshx.Command(target, "true").CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	_ = errors.As(err, &ee)
	return fmt.Errorf("%w: %s: %s\n         set up a key with: ssh-copy-id %s",
		errPeerUnreachable, target, firstLine(strings.TrimSpace(string(out))), target)
}
