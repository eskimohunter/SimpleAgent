//go:build !windows && !linux

package repl

import (
	"errors"
)

// configureConsole is a no-op on non-Windows systems: Unix terminals
// already handle UTF-8 and ANSI color natively. See console_windows.go for
// the Windows implementation that this build tag swaps in.
func configureConsole() {}

// enterRawMode is unsupported on this platform (no termios access via the
// stdlib syscall package): the REPL falls back to cooked line input, and
// plan/build mode is toggled with the /mode command instead of Tab.
func enterRawMode() (func(), error) {
	return func() {}, errors.New("raw input not supported on this platform")
}
