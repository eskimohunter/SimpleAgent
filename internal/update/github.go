package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// hardcoded release source. Releases are published by the release workflow
// (.github/workflows/release.yml) with two assets: simpleagent-linux-amd64
// and simpleagent-windows-amd64.exe, plus SHA256SUMS. The updater lives
// entirely in this package; anything not matching that layout is refused.
const (
	repoOwner = "eskimohunter"
	repoName  = "SimpleAgent"
	repoURL   = "https://api.github.com/repos/" + repoOwner + "/" + repoName
)

// maxBinaryBytes caps how large a replacement binary we are willing to
// download. Huge files are either not SimpleAgent or a corrupted response.
const maxBinaryBytes = 32 << 20

// ErrNoRelease is returned by LatestRelease when the repo has no published
// (non-prerelease) releases - there is simply nothing to update to.
var ErrNoRelease = errors.New("no newer release")

// Updater fetches release metadata and binaries from GitHub. A plain
// http.Client with the standard transport is used: no custom TLS settings,
// nothing the sandbox model client does not also trust.
type Updater struct {
	HTTP   *http.Client
	repo   string
	assets string
}

// NewUpdater builds an updater that talks to the hardcoded repo.
func NewUpdater() *Updater {
	return &Updater{
		HTTP:   &http.Client{Timeout: 30 * time.Second},
		repo:   repoURL,
		assets: "https://github.com/" + repoOwner + "/" + repoName,
	}
}

// Release is the slice of the GitHub releases/latest response the updater
// needs.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// LatestRelease returns the newest published (non-prerelease) release. The
// error is ErrNoRelease when the API reports none.
func (u *Updater) LatestRelease(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching release info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("GitHub release API returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing release info: %w", err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("release info listed no tag name")
	}
	return &rel, nil
}

// binaryName is the release asset name for the platform we are running on.
func binaryName(goos string) string {
	if goos == "windows" {
		return "simpleagent-windows-amd64.exe"
	}
	return "simpleagent-linux-amd64"
}

// FindAsset locates the binary asset for the given GOOS in the release and
// the SHA256SUMS checksum asset. Any other layout is refused.
func (rel *Release) FindAsset(goos string) (binary, checksums Asset, err error) {
	want := binaryName(goos)
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			binary = a
		case "SHA256SUMS":
			checksums = a
		}
	}
	if binary.Name == "" {
		err = fmt.Errorf("release %s has no binary for %s (%s)", rel.TagName, goos, want)
		return
	}
	if checksums.Name == "" {
		err = fmt.Errorf("release %s has no SHA256SUMS asset; refusing to install unverified", rel.TagName)
		return
	}
	return
}

// Download fetches the file at rawURL into memory. The response is capped at
// maxBinaryBytes (we refuse oversized or pathological downloads).
func (u *Updater) Download(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("refusing download URL %q", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", parsed.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinaryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBinaryBytes {
		return nil, fmt.Errorf("downloaded file exceeds %d bytes; refusing", maxBinaryBytes)
	}
	if len(data) == 0 {
		return nil, errors.New("downloaded file is empty")
	}
	return data, nil
}
