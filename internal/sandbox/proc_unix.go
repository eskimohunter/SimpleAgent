//go:build !windows

// Unix counterpart to proc_windows.go: children start in their own process
// group (Setpgid) and the whole group is killed with a negative-PID SIGKILL.
package sandbox

import (
	"os/exec"
	"syscall"
)

// configureProcess puts the child in its own process group so signals sent
// to the harness (e.g. Ctrl+C) do not propagate to the child, and so we can
// target the group for the tree kill below.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree sends SIGKILL to the child's whole process group: the PID
// negated (-pid) addresses the group, catching the shell AND its
// grandchildren, which a plain process kill would miss.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
