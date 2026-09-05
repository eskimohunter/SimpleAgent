//go:build windows

// restart_windows.go is the Windows counterpart of restart_unix.go. Windows
// cannot exec over the running image (the .old dance in swap_windows.go
// already exists because of that), so the new binary is spawned as a child
// with the console stdio inherited, and the caller returns to end the old
// process. Windows job control has no tty-reclaim equivalent, so the child
// keeps the console.
package repl

import (
	"os"
	"os/exec"

	"simpleagent/internal/update"
)

// restartExecutable spawns the binary at exe as a child with the same
// arguments and the console stdio, then returns nil so the caller can end
// the REPL and let the old process exit. The swapped-away .old image is
// cleaned up best-effort once the child is running.
func restartExecutable(exe string) error {
	child := exec.Command(exe, os.Args[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return err
	}
	update.RemoveOldBinary(exe)
	return nil
}
