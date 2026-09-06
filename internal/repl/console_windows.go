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
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const (
	enableVirtualTerminalProcessing = 0x0004
	stdOutputHandle                 = ^uintptr(10)
	stdInputHandle                  = ^uintptr(9)

	// Console input modes cleared by enterRawMode so each key press is
	// delivered immediately instead of being line-buffered and echoed.
	enableProcessedInput = 0x0001 // Ctrl+C handled by the system (off = we see it as a byte)
	enableLineInput      = 0x0002 // Enter delivers the whole line (off = per key)
	enableEchoInput      = 0x0004 // system echo (off = we echo ourselves)
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

// enterRawMode switches the console input to character mode (no line
// buffering, no echo, no system Ctrl+C processing) so the REPL can read
// single keys such as Tab. Returns a function restoring the previous mode.
func enterRawMode() (func(), error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procGetHandle := kernel32.NewProc("GetStdHandle")
	procGetMode := kernel32.NewProc("GetConsoleMode")
	procSetMode := kernel32.NewProc("SetConsoleMode")

	// GetStdHandle(STD_INPUT_HANDLE = -10, i.e. ^uintptr(9)).
	h, _, _ := procGetHandle.Call(stdInputHandle)
	if h == 0 || h == ^uintptr(0) {
		return nil, errors.New("no console input handle")
	}
	var old uint32
	ret, _, _ := procGetMode.Call(h, uintptr(unsafe.Pointer(&old)))
	if ret == 0 {
		return nil, errors.New("GetConsoleMode failed")
	}
	raw := old &^ (enableProcessedInput | enableLineInput | enableEchoInput)
	// Check the BOOL return value, not the last-error value: Win32 APIs
	// leave GetLastError() stale on success, so testing the err slot of
	// SyscallN would report failure (and take the cooked fallback) even
	// though the mode was applied - leaving the console raw with echo
	// off and no UI drawing a prompt. Same pattern as GetConsoleMode
	// above.
	ret, _, _ := procSetMode.Call(h, uintptr(raw))
	if ret == 0 {
		return nil, errors.New("SetConsoleMode failed")
	}
	return func() {
		_, _, _ = procSetMode.Call(h, uintptr(old))
	}, nil
}
