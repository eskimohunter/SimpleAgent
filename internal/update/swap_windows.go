//go:build windows

// swap_windows.go replaces the running executable, which Windows will not
// let you overwrite (sharing violation on the running image). The dance is
// the rename trick: move the running exe aside to exe.old, move the new
// binary into its place, then (after the child has spawned) delete the
// .old file. If either rename fails the old binary is restored.
package update

import (
	"fmt"
	"os"
)

// replaceExecutable swaps the downloaded and verified tmp file for the
// running binary via the .old rename dance. On failure the original exe is
// put back so the harness keeps running.
func replaceExecutable(tmp, exe string) error {
	old := exe + ".old"
	// Remove a stale .old from a previous crash; failure here just means
	// there was none.
	_ = os.Remove(old)

	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("renaming running binary aside: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		// Put the old binary back before reporting the failure.
		restoreErr := os.Rename(old, exe)
		if restoreErr != nil {
			return fmt.Errorf("replacing binary failed (%v) and restoring the old one failed too: %v", err, restoreErr)
		}
		return fmt.Errorf("placing new binary: %w", err)
	}
	return nil
}

// RemoveOldBinary cleans up the .old file left by the swap. Called after the
// replacement process has been spawned; for the brief window while the old
// process is still exiting the remove may fail, which is fine - the next
// update's swap starts by clearing any stale .old.
func RemoveOldBinary(exe string) {
	_ = os.Remove(exe + ".old")
}
