//go:build !windows

package repl

// configureConsole is a no-op on non-Windows systems: Unix terminals
// already handle UTF-8 and ANSI color natively. See console_windows.go for
// the Windows implementation that this build tag swaps in.
func configureConsole() {}
