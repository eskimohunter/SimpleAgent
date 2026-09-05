package update

import (
	"strconv"
	"strings"
)

// version is a parsed semver-ish release number: major, minor and patch
// integers plus an optional pre-release suffix (e.g. "rc1" or "1-gabc123").
// Only the numeric triple is compared; suffixes signal "not a clean release"
// but never outrank a released version.
type version struct {
	major, minor, patch int
	suffix              string // "" for a clean release
	valid               bool
}

// parseVersion splits a version string into the numeric triple and suffix.
// Accepted shapes: "v1.2.3", "1.2.3", "v1.2.3-rc1" (and anything after the
// third dot-separated component, e.g. git describe's "v0.1.0-3-gabc123-dirty",
// becomes the suffix). Returns valid=false for anything else.
func parseVersion(s string) version {
	s = strings.TrimSpace(strings.TrimPrefix(s, "v"))
	rest := s
	suffix := ""
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		rest, suffix = rest[:i], rest[i+1:]
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return version{}
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}
		}
		nums[i] = n
	}
	return version{major: nums[0], minor: nums[1], patch: nums[2], suffix: suffix, valid: true}
}

// compare orders two parsed versions: -1 if a < b, 0 if equal, 1 if a > b.
// The suffix is ignored, so v0.2.0-rc1 compares equal to v0.2.0; callers
// treat "not newer" as "up to date" (see needsUpdate).
func compare(a, b version) int {
	if !a.valid || !b.valid {
		return 0
	}
	for _, pair := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// needsUpdate reports whether the released upstream version is strictly
// newer than the running one. Unparseable versions (e.g. "dev" fallback
// builds) are treated as equal, so an update is only ever offered on a
// real version comparison.
func needsUpdate(current, latest version) bool {
	return compare(latest, current) > 0
}

// NewerAvailable is the exported wrapper used by the REPL: it reports
// whether the released tag (e.g. "v0.2.0") is strictly newer than the
// running version string (e.g. "v0.1.0-3-gabc123-dirty"). Unparseable
// inputs never count as newer.
func NewerAvailable(current, latest string) bool {
	return needsUpdate(parseVersion(current), parseVersion(latest))
}
