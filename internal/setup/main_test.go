package setup_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/grillermo/git-sync/internal/secret"
)

// TestMain lets this test binary double as the askpass helper sshx.Command
// arms via SSH_ASKPASS during EnsureAuth's pre-install password verification.
// sshx writes a shim that execs the currently running binary as
// `<self> askpass <account>`; in these tests that binary IS this compiled
// test binary (there is no installed git-sync yet to point at), so it has to
// understand that invocation itself, mirroring cmd/git-sync's cmdAskpass.
// Checked before anything from the testing package runs, so it never
// recurses into a full test run.
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "askpass" {
		pw, err := secret.Get(os.Args[2])
		if err != nil {
			os.Exit(1)
		}
		fmt.Println(string(pw))
		os.Exit(0)
	}
	os.Exit(m.Run())
}
