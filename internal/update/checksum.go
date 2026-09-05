package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// checksumFor extracts the lowercase hex SHA-256 digest for the asset named
// want from a release SHA256SUMS body. The format matches what `sha256sum`
// emits on the release machine ("<hex>  <name>"), with whitespace between
// the hex and the name.
func checksumFor(sumBody, want string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(sumBody))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] != want {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != sha256.Size*2 {
			return "", fmt.Errorf("SHA256SUMS entry for %s is not a valid SHA-256", want)
		}
		return strings.ToLower(fields[0]), nil
	}
	return "", fmt.Errorf("SHA256SUMS contains no entry for %s", want)
}

// Verify checks that data matches the SHA-256 recorded for assetName in the
// release's SHA256SUMS file. On any mismatch the file is not installed.
func (u *Updater) Verify(ctx context.Context, sumsURL, assetName string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sumsURL, nil)
	if err != nil {
		return err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("fetching SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SHA256SUMS download failed: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	want, err := checksumFor(string(body), assetName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch: downloaded %s has SHA-256 %s, release lists %s", assetName, got, want)
	}
	return nil
}
