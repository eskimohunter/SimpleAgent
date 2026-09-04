//go:build windows

// This file is only compiled on Windows (see the build tag above; proc_unix
// is the counterpart). It makes child processes start in their own process
// group with a hidden window, and kills the whole group on timeout/cancel -
// a bare process kill would orphan the shell's grandchildren.
package sandbox

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// createNewProcessGroup is the CREATE_NEW_PROCESS_GROUP flag: the child gets
// its own process group, which lets us signal the group (and only the group)
// later.
const createNewProcessGroup = 0x00000200

// configureProcess sets the Windows process attributes before cmd.Start:
// HideWindow keeps the child console from flashing up, and the new process
// group keeps the child separate from the harness's own console group (so
// Ctrl+C on the harness never reaches the child directly).
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup,
	}
}

// killProcessTree force-kills the command and everything it spawned via the
// built-in taskkill tool (/T kills the tree, /F forces). A 10-second cap
// keeps a stuck taskkill from blocking the turn forever.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = kill.Run()
	return nil
}
