//go:build unix

package syncer

import "syscall"

// detachAttr puts the child in its own session so it outlives this process
// and is unaffected by signals sent to git's process group.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
