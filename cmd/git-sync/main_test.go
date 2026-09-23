package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/lock"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestRunUnknownSubcommand(t *testing.T) {
	var out strings.Builder
	code := run([]string{"wat"}, &out, &out)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "unknown subcommand") {
		t.Errorf("output = %q, want it to mention the unknown subcommand", out.String())
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out strings.Builder
	code := run(nil, &out, &out)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	for _, want := range []string{"install", "uninstall", "report"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestRunUsageHidesMachineSubcommands(t *testing.T) {
	// hook/push/receive are invoked by the hook and by ssh, never typed by a
	// human. Keep them out of the usage text so the CLI stays legible.
	var out strings.Builder
	run(nil, &out, &out)
	for _, hidden := range []string{"receive", "hook"} {
		if strings.Contains(out.String(), hidden) {
			t.Errorf("usage should not advertise %q:\n%s", hidden, out.String())
		}
	}
}

func TestHookPreCommitBlocksWhileReceiving(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	defer l.Release()
	testutil.Chdir(t, repo)

	var out, errBuf bytes.Buffer
	if code := run([]string{"hook", "pre-commit"}, &out, &errBuf); code != 1 {
		t.Fatalf("hook pre-commit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String()+out.String(), "laptop.local") {
		t.Errorf("no explanation printed: %q %q", out.String(), errBuf.String())
	}
}

func TestHookPrePushBlocksWhileReceiving(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	defer l.Release()
	testutil.Chdir(t, repo)

	if code := run([]string{"hook", "pre-push"}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("hook pre-push = %d, want 1", code)
	}
}

func TestUnlockClearsTheLockAndNamesTheHolder(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	l, _ := lock.AcquireFrom("group/proj", "laptop.local", time.Second)
	_ = l
	testutil.Chdir(t, repo)

	var out bytes.Buffer
	if code := run([]string{"unlock"}, &out, io.Discard); code != 0 {
		t.Fatalf("unlock = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "laptop.local") {
		t.Errorf("unlock should name the holder, got %q", out.String())
	}
	if _, held := lock.Held("group/proj"); held {
		t.Error("unlock left the lock in place")
	}
}

func TestUnlockOnAnUnlockedRepoIsFine(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	testutil.Chdir(t, repo)

	if code := run([]string{"unlock", "group/proj"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("unlock = %d, want 0", code)
	}
}
