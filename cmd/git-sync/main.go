// Command git-sync keeps git repositories in sync between two machines.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `git-sync keeps git repos in sync between two machines.

Usage:
  git-sync install <base_dir>   pick repos under base_dir and set up both machines
  git-sync uninstall [--purge]  stop syncing (--purge also deletes config and history)
  git-sync report [flags]       browse sync activity, grouped by repo

Run a command with -h for its flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code.
// Exit codes: 0 ok, 1 failure, 2 usage error, 3 repo not on this machine.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "install":
		return cmdInstall(args[1:], stdout, stderr)
	case "uninstall":
		return cmdUninstall(args[1:], stdout, stderr)
	case "report":
		return cmdReport(args[1:], stdout, stderr)

	// Machine-invoked, deliberately absent from the usage text.
	case "hook":
		return cmdHook(args[1:], stderr)
	case "push":
		return cmdPush(args[1:], stderr)
	case "receive":
		return cmdReceive(args[1:], stderr)
	// Invoked by ssh (askpass) and by the peer over ssh (savepass); see Task 12.
	case "askpass":
		return cmdAskpass(args[1:], stdout, stderr)
	case "savepass":
		return cmdSavepass(args[1:], os.Stdin, stderr)

	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}
