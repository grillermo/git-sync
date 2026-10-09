//go:build unix

package syncer

import (
	"errors"
	"os"
	"syscall"
)

// detachAttr puts the child in its own session so it outlives this process
// and is unaffected by signals sent to git's process group.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// groupAttr starts a child as the leader of a new process group, so
// killGroup can stop it together with everything it started.
func groupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the process group p leads.
func killGroup(p *os.Process) error {
	err := syscall.Kill(-p.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
