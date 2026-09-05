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

// LatestRelease returns the newest release that can actually be installed
// for the given GOOS: the latest non-prerelease release (releases/latest)
// when it carries the platform binary and SHA256SUMS, otherwise the
// highest-versioned complete release from the full list (prereleases
// included). The two fallbacks exist because a repo that only publishes
// prereleases - as SimpleAgent does while it is still early - otherwise
// gives /update nothing to find, and a broken top release (partial asset
// upload) otherwise hard-fails the update even though an older complete
// release exists. The error is ErrNoRelease when the repo has no releases
// at all.
func (u *Updater) LatestRelease(ctx context.Context, goos string) (*Release, error) {
	rel, err := u.latestStable(ctx)
	if err == nil && rel.complete(goos) {
		return rel, nil
	}
	if err != nil && !errors.Is(err, ErrNoRelease) {
		return nil, err
	}
	return u.highestRelease(ctx, goos)
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

// listReleases fetches the release list, drafts and prereleases included.
// GitHub orders it newest first, but the caller deliberately ranks by
// version instead, so partial ordering is fine. Note the list is paginated
// (30 per page, first page only): a repo with more than 30 releases must
// keep its tags version-monotonic for the oldest entries to be visible.
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
// full list that carries a complete asset set for goos (platform binary +
// SHA256SUMS), so a partially-uploaded release cannot block updates. If no
// release is complete, the highest-versioned one is returned anyway and
// FindAsset reports which asset is missing. Drafts are skipped; GitHub does
// not list drafts to unauthenticated callers, but the check keeps the
// behavior correct if the updater ever gets authenticated.
func (u *Updater) highestRelease(ctx context.Context, goos string) (*Release, error) {
	rels, err := u.listReleases(ctx)
	if err != nil {
		return nil, err
	}
	var best, bestComplete *Release
	var bestV, bestCompleteV version
	for i := range rels {
		rel := &rels[i]
		if rel.Draft {
			continue
		}
		v := parseVersion(rel.TagName)
		if better(rel, v, best, bestV) {
			best = rel
			bestV = v
		}
		if rel.complete(goos) && better(rel, v, bestComplete, bestCompleteV) {
			bestComplete = rel
			bestCompleteV = v
		}
	}
	if bestComplete != nil {
		return bestComplete, nil
	}
	if best == nil {
		return nil, ErrNoRelease
	}
	return best, nil
}

// better ranks a candidate release against the current best: a parseable
// version always outranks an unparseable tag (compare treats invalid
// inputs as equal, so that case must be decided here); otherwise the
// higher numeric version wins; ties keep the earlier entry.
func better(cand *Release, candV version, best *Release, bestV version) bool {
	if best == nil {
		return true
	}
	if bestV.valid != candV.valid {
		return candV.valid
	}
	return compare(candV, bestV) > 0
}

// complete reports whether the release carries the platform binary asset
// and SHA256SUMS - everything Install needs. Name presence is enough here;
// FindAsset produces the detailed refusal when only an incomplete release
// is available.
func (rel *Release) complete(goos string) bool {
	want := binaryName(goos)
	hasBin, hasSums := false, false
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			hasBin = true
		case "SHA256SUMS":
			hasSums = true
		}
	}
	return hasBin && hasSums
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
