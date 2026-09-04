//go:build linux

// Raw (character) console input for Linux terminals: the terminal is put
// into cbreak-like mode so the REPL can read single keystrokes (Tab toggles
// plan/build mode) without waiting for Enter. Uses the termios ioctls via
// the stdlib syscall package - no external dependencies. Windows has its own
// implementation in console_windows.go; other platforms get the cooked-mode
// fallback in console_other.go.
package repl

import (
	"os"
	"syscall"
	"unsafe"
)

func configureConsole() {}

// enterRawMode switches stdin's terminal from canonical (line) mode to
// character mode: echo off, line buffering off, signal generation off and
// no input post-processing, so each key press is delivered as it happens.
// It returns a function that restores the previous terminal state.
func enterRawMode() (func(), error) {
	fd := os.Stdin.Fd()
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	return func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
	}, nil
}
