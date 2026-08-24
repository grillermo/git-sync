package sshx_test

import (
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/secret"
	"github.com/grillermo/git-sync/internal/sshx"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestNoStoredPasswordMeansBatchMode(t *testing.T) {
	// Nothing can answer a prompt in a detached hook; fail fast instead.
	testutil.NewSandbox(t)

	cmd := sshx.Command("tester@peer.example", "some remote command")
	if !containsArg(cmd.Args, "BatchMode=yes") {
		t.Errorf("Args = %v, want BatchMode=yes", cmd.Args)
	}
	if containsArg(cmd.Args, "BatchMode=no") {
		t.Errorf("Args = %v, must not contain BatchMode=no with no stored password", cmd.Args)
	}
}

func TestAStoredPasswordArmsTheAskpassHelper(t *testing.T) {
	// SSH_ASKPASS set, SSH_ASKPASS_REQUIRE=force, BatchMode=no.
	testutil.NewSandbox(t)
	if err := secret.Set("tester@peer.example", []byte("hunter2")); err != nil {
		t.Fatalf("secret.Set: %v", err)
	}

	cmd := sshx.Command("tester@peer.example", "some remote command")
	if !containsArg(cmd.Args, "BatchMode=no") {
		t.Errorf("Args = %v, want BatchMode=no", cmd.Args)
	}
	if !containsEnv(cmd.Env, "SSH_ASKPASS="+config.AskpassPath()) {
		t.Errorf("Env = %v, want SSH_ASKPASS=%s", cmd.Env, config.AskpassPath())
	}
	if !containsEnv(cmd.Env, "SSH_ASKPASS_REQUIRE=force") {
		t.Errorf("Env = %v, want SSH_ASKPASS_REQUIRE=force", cmd.Env)
	}
}

func TestThePasswordIsNeverInArgvOrTheEnvironment(t *testing.T) {
	// The strongest assertion in this task: grep the whole command line and
	// environment for the password and find nothing. It travels only from the
	// keychain, through the askpass helper's stdout, into ssh.
	testutil.NewSandbox(t)
	if err := secret.Set("tester@peer.example", []byte("hunter2")); err != nil {
		t.Fatalf("secret.Set: %v", err)
	}

	cmd := sshx.Command("tester@peer.example", "some remote command")
	for _, a := range cmd.Args {
		if strings.Contains(a, "hunter2") {
			t.Fatalf("Args = %v contains the password", cmd.Args)
		}
	}
	for _, e := range cmd.Env {
		if strings.Contains(e, "hunter2") {
			t.Fatalf("Env = %v contains the password", cmd.Env)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
