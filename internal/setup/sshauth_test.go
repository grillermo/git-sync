package setup_test

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/secret"
	"github.com/grillermo/git-sync/internal/setup"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestPeerNeedingAPasswordIsDetected(t *testing.T) {
	// ssh exits 255 having said "Permission denied (publickey,password)".
	// That is not the same as "host unreachable", and install must not
	// conflate them: one is answerable, the other is not.
	testutil.NewSandbox(t)
	sb := testutil.NewSandbox(t)
	sb.StubSSHFailing(255, "tester@peer.example: Permission denied (publickey,password).")

	_, err := setup.Probe("tester@peer.example")
	if !setup.IsPasswordRequired(err) {
		t.Errorf("err = %v, want it classified as needing a password", err)
	}
	if setup.IsPeerUnreachable(err) {
		t.Error("a reachable host that wants a password is not unreachable")
	}
}

func TestEnsureAuthStoresAWorkingPassword(t *testing.T) {
	sb := testutil.NewSandbox(t)
	// The stub rejects everything until it sees the right password, so this
	// asserts the verify-before-store rule rather than just the plumbing.
	sb.StubSSHPassword("hunter2")

	err := setup.EnsureAuth("tester@peer.example",
		strings.NewReader("hunter2\n"), io.Discard)
	if err != nil {
		t.Fatalf("EnsureAuth: %v", err)
	}
	got, err := secret.Get("tester@peer.example")
	if err != nil || string(got) != "hunter2" {
		t.Errorf("stored %q, %v; want the password that worked", got, err)
	}
}

func TestEnsureAuthFailsIfTheKeychainDidNotKeepIt(t *testing.T) {
	// A write that reports success but does not persist - a locked keychain,
	// a denied prompt - must fail the install here, loudly, rather than at
	// the next commit in the background where nobody is watching.
	sb := testutil.NewSandbox(t)
	sb.StubSSHPassword("hunter2")
	t.Setenv("GITSYNC_SECRET_BACKEND", "blackhole") // accepts writes, stores nothing

	err := setup.EnsureAuth("tester@peer.example", strings.NewReader("hunter2\n"), io.Discard)
	if err == nil {
		t.Fatal("expected an error when the password did not persist")
	}
	if !strings.Contains(err.Error(), "keychain") {
		t.Errorf("err = %v; it should say the password could not be saved", err)
	}
}

func TestEnsureAuthDoesNotStoreAPasswordThatFails(t *testing.T) {
	// Storing an unverified password is worse than storing none: every later
	// sync fails in the background with a credential that was never right.
	sb := testutil.NewSandbox(t)
	sb.StubSSHPassword("hunter2")

	err := setup.EnsureAuth("tester@peer.example",
		strings.NewReader("wrong\nwrong\nwrong\n"), io.Discard)
	if err == nil {
		t.Fatal("expected an error after the retries are used up")
	}
	if secret.Has("tester@peer.example") {
		t.Error("a password that never worked must not be stored")
	}
}

func TestEnsureAuthRetriesAfterATypo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHPassword("hunter2")

	if err := setup.EnsureAuth("tester@peer.example",
		strings.NewReader("typo\nhunter2\n"), io.Discard); err != nil {
		t.Fatalf("a typo should not end the install: %v", err)
	}
	if !secret.Has("tester@peer.example") {
		t.Error("the second attempt worked and should have been stored")
	}
}

func TestEnsureAuthWithNoTerminalExplainsInsteadOfHanging(t *testing.T) {
	// A scripted install cannot answer a prompt. Say what to do - set up a
	// key, or run install on a terminal - and stop.
	sb := testutil.NewSandbox(t)
	sb.StubSSHFailing(255, "Permission denied (publickey,password).")

	var out strings.Builder
	err := setup.EnsureAuth("tester@peer.example", nil, &out)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ssh key") {
		t.Errorf("err = %v; it should point at the fix", err)
	}
}

func TestEnsureAuthIsSkippedWhenKeysAlreadyWork(t *testing.T) {
	// The common case: never ask for a password we do not need.
	sb := testutil.NewSandbox(t)
	sb.StubSSHScripted(map[string]string{"uname": localUname(), "$HOME": "/home/peer"}, 0)

	if err := setup.EnsureAuth("tester@peer.example", nil, io.Discard); err != nil {
		t.Fatalf("EnsureAuth: %v", err)
	}
	if secret.Has("tester@peer.example") {
		t.Error("key auth works; nothing should have been stored")
	}
}

func TestThePasswordIsNeverWrittenToConfigOrTheDebugLog(t *testing.T) {
	sb := testutil.NewSandbox(t)
	sb.StubSSHPassword("hunter2")
	_ = setup.EnsureAuth("tester@peer.example", strings.NewReader("hunter2\n"), io.Discard)

	self := testutil.WriteScript(t, sb, "git-sync-fake", "#!/bin/sh\nexit 0\n")
	_ = setup.Install(setup.Options{
		BaseDir: sb.BaseDir, PeerHost: "peer.example", PeerUser: "tester",
		Self: self, NoPeer: true, Out: io.Discard,
	})

	for _, p := range []string{config.Path(), config.DebugLogPath(), config.ActivityPath()} {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "hunter2") {
			t.Errorf("%s contains the password", p)
		}
	}
}
