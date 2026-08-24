package main

import (
	"fmt"
	"io"
	"os"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/syncer"
)

func cmdInstall(args []string, stdout, stderr io.Writer) int   { return 1 }
func cmdUninstall(args []string, stdout, stderr io.Writer) int { return 1 }
func cmdReport(args []string, stdout, stderr io.Writer) int    { return 1 }

func cmdHook(args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "post-commit" {
		fmt.Fprintln(stderr, "usage: git-sync hook post-commit")
		return 2
	}
	wd, err := os.Getwd()
	if err != nil {
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		self = config.BinPath()
	}
	if err := syncer.Hook(wd, func(rel string) error {
		return syncer.SpawnDetached(self, rel)
	}); err != nil {
		// Never fail the commit over a sync problem.
		activity.AppendDebug("hook: " + err.Error())
	}
	return 0
}

func cmdPush(args []string, stderr io.Writer) int    { return 1 }
func cmdReceive(args []string, stderr io.Writer) int { return 1 }

func cmdAskpass(args []string, stdout, stderr io.Writer) int           { return 1 }
func cmdSavepass(args []string, stdin io.Reader, stderr io.Writer) int { return 1 }
