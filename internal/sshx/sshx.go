// Package sshx builds the ssh commands git-sync runs. git-sync is key-only:
// BatchMode=yes everywhere, because nothing in the sync path has a terminal
// that could answer a password prompt.
package sshx

import "os/exec"

// Command builds an ssh to account running remote.
//
// ConnectTimeout bounds a peer that is off when we dial it. The keepalives
// bound one that goes away after we connected - a laptop lid closing mid
// receive. Without them a dead TCP connection can leave the background push
// waiting forever. With them ssh gives up after about a minute of silence and
// exits 255, the same as an unreachable peer.
func Command(account, remote string) *exec.Cmd {
	return exec.Command("ssh",
		"-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "BatchMode=yes",
		account, remote)
}
