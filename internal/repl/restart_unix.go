//go:build !windows

// restart_unix.go is the Unix counterpart of restart_windows.go. On Unix
// the new binary is exec'd IN PLACE: the running process image is replaced
// (system call execve), so the restarted harness keeps the same PID, the
// same session and the same foreground process group. That matters: if the
// old process simply spawned a child and exited, the parent shell's job
// control would reclaim the terminal (tcsetpgrp), the child would land in a
// background process group, and its first terminal access would stop it
// with SIGTTIN/SIGTTOU - the REPL appearing to "drop back to the shell".
// execve never returns on success, so callers must not run code after it.
package repl

import (
	"os"
	"syscall"
)

// restartExecutable replaces the current process with the binary at exe
// (typically the path of the running executable, now updated in place). On
// success it does not return. Flags are forwarded unchanged; the process
// environment is inherited as-is.
func restartExecutable(exe string) error {
	return syscall.Exec(exe, append([]string{exe}, os.Args[1:]...), os.Environ())
}
