package main

import (
	"strings"
	"testing"
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
	for _, hidden := range []string{"receive", "hook", "askpass", "savepass"} {
		if strings.Contains(out.String(), hidden) {
			t.Errorf("usage should not advertise %q:\n%s", hidden, out.String())
		}
	}
}
