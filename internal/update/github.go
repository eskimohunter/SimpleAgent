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

// ErrNoRelease is returned by LatestRelease when the repo has no releases
// at all (not even a prerelease) - there is simply nothing to update to.
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

// Release is the slice of the GitHub release response the updater needs.
type Release struct {
	TagName string  `json:"tag_name"`
	Draft   bool    `json:"draft"`
	Assets  []Asset `json:"assets"`
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// LatestRelease returns the newest release we can offer: the latest
// non-prerelease release (releases/latest) when the repo has one, otherwise
// the highest-versioned release from the full list. The fallback matters
// because a repo that only publishes prereleases - as SimpleAgent does while
// it is still early - otherwise gives /update nothing to find, even though
// newer builds exist. The error is ErrNoRelease when the repo has no
// releases at all.
func (u *Updater) LatestRelease(ctx context.Context) (*Release, error) {
	rel, err := u.latestStable(ctx)
	if err == nil {
		return rel, nil
	}
	if !errors.Is(err, ErrNoRelease) {
		return nil, err
	}
	return u.highestRelease(ctx)
}

// latestStable queries the releases/latest endpoint, which never returns
// drafts or prereleases.
func (u *Updater) latestStable(ctx context.Context) (*Release, error) {
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

// listReleases fetches the full release list, drafts and prereleases
// included. GitHub orders it newest first, but the caller should not rely
// on that alone.
func (u *Updater) listReleases(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.repo+"/releases", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching release list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("GitHub release API returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var rels []Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&rels); err != nil {
		return nil, fmt.Errorf("parsing release list: %w", err)
	}
	return rels, nil
}

// highestRelease picks the highest-versioned non-draft release from the
// full list; drafts are skipped (they are not user-facing), prereleases are
// kept. Versions are compared numerically, so the list order does not
// matter; ties keep the earlier entry.
func (u *Updater) highestRelease(ctx context.Context) (*Release, error) {
	rels, err := u.listReleases(ctx)
	if err != nil {
		return nil, err
	}
	var best *Release
	var bestV version
	for i := range rels {
		rel := &rels[i]
		if rel.Draft {
			continue
		}
		v := parseVersion(rel.TagName)
		if best == nil {
			best = rel
			bestV = v
			continue
		}
		// A parseable version always outranks an unparseable tag (compare
		// treats invalid inputs as equal, so that case must be decided
		// here); between two unparseable tags the earlier entry wins.
		switch {
		case !bestV.valid && v.valid:
			best = rel
			bestV = v
		case bestV.valid && !v.valid:
		case compare(v, bestV) > 0:
			best = rel
			bestV = v
		}
	}
	if best == nil {
		return nil, ErrNoRelease
	}
	return best, nil
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
