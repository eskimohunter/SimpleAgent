//go:build windows

package repl

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	enableVirtualTerminalProcessing = 0x0004
	stdOutputHandle                 = ^uintptr(10)
)

func configureConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procSetOutCP := kernel32.NewProc("SetConsoleOutputCP")
	procSetCP := kernel32.NewProc("SetConsoleCP")
	procGetHandle := kernel32.NewProc("GetStdHandle")
	procGetMode := kernel32.NewProc("GetConsoleMode")
	procSetMode := kernel32.NewProc("SetConsoleMode")

	_, _, _ = procSetOutCP.Call(65001)
	_, _, _ = procSetCP.Call(65001)

	if os.Getenv("NO_COLOR") != "" {
		return
	}
	h, _, _ := procGetHandle.Call(stdOutputHandle)
	if h == 0 || h == ^uintptr(0) {
		return
	}
	var mode uint32
	ret, _, _ := procGetMode.Call(h, uintptr(unsafe.Pointer(&mode)))
	if ret == 0 {
		return
	}
	_, _, _ = procSetMode.Call(h, uintptr(mode|enableVirtualTerminalProcessing))
}
