// Package sshx builds the ssh commands git-sync runs. git-sync is key-only:
// BatchMode=yes everywhere, because nothing in the sync path has a terminal
// that could answer a password prompt.
package sshx

import "os/exec"

func Command(account, remote string) *exec.Cmd {
	return exec.Command("ssh",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		account, remote)
}
