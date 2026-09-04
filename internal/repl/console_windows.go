//go:build windows

// The Windows console needs two fixes that Unix terminals get for free:
//  1. the legacy console uses a non-UTF-8 codepage, which garbles UTF-8
//     output, so we switch input and output to codepage 65001 (UTF-8);
//  2. ANSI colors are off by default, so we enable "virtual terminal
//     processing" on the stdout handle - this makes the \x1b[...m escapes
//     produced by TextUI.paint render as colors instead of garbage.
//
// The functions are called from syscall.NewLazyDLL, which loads kernel32.dll
// on demand and lets us call Win32 APIs without cgo or external deps.
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

	// 65001 = UTF-8 codepage.
	_, _, _ = procSetOutCP.Call(65001)
	_, _, _ = procSetCP.Call(65001)

	if os.Getenv("NO_COLOR") != "" {
		return
	}
	// GetStdHandle(STD_OUTPUT_HANDLE = -11, i.e. ^uintptr(10)).
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
