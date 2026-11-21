//go:build !windows

package observe

import (
	"os/exec"
	"syscall"
)

// setGroup starts the child as the leader of a new process group.
func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills every process of the child's group. The group id stays valid while any member
// lives, so a reaped leader whose grandchild survives is still covered; an empty group is ESRCH.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
