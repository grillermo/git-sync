package main

import "io"

func cmdInstall(args []string, stdout, stderr io.Writer) int   { return 1 }
func cmdUninstall(args []string, stdout, stderr io.Writer) int { return 1 }
func cmdReport(args []string, stdout, stderr io.Writer) int    { return 1 }
func cmdHook(args []string, stderr io.Writer) int              { return 1 }
func cmdPush(args []string, stderr io.Writer) int              { return 1 }
func cmdReceive(args []string, stderr io.Writer) int           { return 1 }

func cmdAskpass(args []string, stdout, stderr io.Writer) int           { return 1 }
func cmdSavepass(args []string, stdin io.Reader, stderr io.Writer) int { return 1 }
