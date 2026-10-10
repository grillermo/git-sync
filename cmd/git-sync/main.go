// Command git-sync keeps git repositories in sync across a mesh of two or
// more machines.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `git-sync keeps git repos in sync between two or more machines.

Usage:
  git-sync install <base_dir>          pick repos under base_dir and set up every machine in the mesh
  git-sync install                     sync just the repo you are in; asks which machines to sync it with
  git-sync uninstall [--purge] [--local]  stop syncing (mesh-wide by default; --local limits it to this machine; --purge also deletes config and history)
  git-sync report [flags]              browse sync activity, grouped by repo
  git-sync unlock [<repo>]             clear a stuck sync lock (default: this repo)
  git-sync activate [<repo>]           run a repo's ./activate now (default: this repo)
  git-sync service install|uninstall|status [--local]
                                       the login service that tells the mesh a machine is back

Run a command with -h for its flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code.
// Exit codes: 0 ok, 1 failure, 2 usage error, 3 repo not on this machine,
// 4 receive could not fetch.
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
	case "unlock":
		return cmdUnlock(args[1:], stdout, stderr)
	case "activate":
		return cmdActivate(args[1:], stdout, stderr)
	case "service":
		return cmdService(args[1:], stdout, stderr)

	// Machine-invoked, deliberately absent from the usage text.
	case "hook":
		return cmdHook(args[1:], stderr)
	case "push":
		return cmdPush(args[1:], stderr)
	case "receive":
		return cmdReceive(args[1:], stderr)
	case "retry":
		return cmdRetry(args[1:], stderr)
	case "announce":
		return cmdAnnounce(args[1:], stderr)
	case "watch":
		return cmdWatch(args[1:], stderr)

	case "status":
		return cmdStatus(args[1:], stdout, stderr)

	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}
