//go:build windows

package observe

import "os/exec"

// setGroup does nothing on Windows: only the direct child is killed there (design §3.1).
func setGroup(*exec.Cmd) {}

// killGroup kills the direct child.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
