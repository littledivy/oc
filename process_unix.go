//go:build unix

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// setSysProcAttr configures the command to run in its own process group
// so we can signal the entire group (command + children).
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killCmdGroup sends SIGTERM to the process group, then SIGKILL after 200ms.
func killCmdGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(200*time.Millisecond, func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
}
