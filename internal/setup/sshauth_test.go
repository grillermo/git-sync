package setup_test

import (
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestReachableAcceptsAPeerThatAnswers(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSH(0)
	if err := setup.Reachable("tester@peer.example"); err != nil {
		t.Fatalf("Reachable: %v", err)
	}
}

func TestReachableReportsAPeerThatRefuses(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHFailing(255, "Permission denied (publickey).")
	err := setup.Reachable("tester@peer.example")
	if err == nil || !setup.IsPeerUnreachable(err) {
		t.Fatalf("Reachable = %v, want an unreachable error", err)
	}
	if !strings.Contains(err.Error(), "ssh-copy-id") {
		t.Errorf("error should tell the user how to fix it, got %q", err)
	}
}
