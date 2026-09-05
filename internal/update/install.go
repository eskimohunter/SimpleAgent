package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Install downloads the release's binary for the given GOOS, verifies it
// against the release's SHA256SUMS, stages it next to exe and swaps it in
// for the running binary. The stage file is created in the executable's own
// directory so the final os.Rename always stays on one filesystem (a
// cross-device rename would fail), and it is chmod'd to the running
// binary's permission bits BEFORE the swap: a rename preserves the source
// file's mode, so without this the freshly installed binary would lose its
// execute bit and the restart would fail with EACCES.
//
// Returns the verified SHA-256 of the installed binary.
//
// Ordering matters for safety: the checksum is checked BEFORE anything in
// the filesystem changes, and the replacement only happens after the file
// is fully staged.
func (u *Updater) Install(ctx context.Context, rel *Release, goos, exe string) (string, error) {
	binary, sums, err := rel.FindAsset(goos)
	if err != nil {
		return "", err
	}
	data, err := u.Download(ctx, binary.URL)
	if err != nil {
		return "", err
	}
	if err := u.Verify(ctx, sums.URL, binary.Name, data); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	mode := fs.FileMode(0o755)
	if fi, err := os.Stat(exe); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".simpleagent-update-*")
	if err != nil {
		return "", fmt.Errorf("staging new binary: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", fmt.Errorf("setting new binary permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("staging new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("staging new binary: %w", err)
	}

	if err := replaceExecutable(tmpPath, exe); err != nil {
		return "", fmt.Errorf("installing new binary: %w", err)
	}
	return hexSum, nil
}
