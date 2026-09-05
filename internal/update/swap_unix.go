//go:build !windows

// swap_unix.go is the Unix counterpart of swap_windows.go. Unix permits
// renaming over a file that is currently executing, so the replacement is a
// single atomic os.Rename: either the new binary is in place or the old one
// is untouched, with no window where both exist.
package update

import "os"

// replaceExecutable atomically moves the downloaded and verified tmp file
// over the running binary.
func replaceExecutable(tmp, exe string) error {
	return os.Rename(tmp, exe)
}

// RemoveOldBinary is a no-op on Unix: the atomic rename never produced an
// .old file, but the restart path calls it uniformly.
func RemoveOldBinary(string) {}
