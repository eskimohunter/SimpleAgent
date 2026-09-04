// Package sandbox implements the machine-enforced containment boundary of
// the harness. Everything the model's file tools touch passes through
// Root.Resolve (paths.go), which rejects any path that could escape the
// project root - including Windows device names, alternate data streams,
// UNC paths, and symlink/junction escapes. See docs/security.md for the
// threat model this defends against.
//
// The package also provides the size-capped file operations (fs.go) and the
// process isolation for run_command (exec.go + the platform proc files).
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Sentinel errors returned by Root.Resolve. The engine prefixes them with
// "ERROR:" so the model sees a concise explanation. They are exported so
// tests can assert on the failure class.
var (
	ErrOutside   = errors.New("path escapes project root")
	ErrProtected = errors.New("path refers to harness state (.agent), which is protected")
	ErrDevice    = errors.New("path contains a Windows device name or alternate stream")
	ErrSymlink   = errors.New("path contains an unresolvable or escaping symlink")
)

// reservedDeviceNames lists the device names Windows treats specially: any
// path that ends in one of these (e.g. "C:\NUL") addresses a device, not a
// regular file. We reject them so the sandbox can never write through a
// device alias. The stem check also covers names like "NUL.txt", which
// Windows resolves to the same device.
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Root is the confinement anchor: the project root directory in absolute
// form, plus a flag recording whether the current OS is Windows (Windows
// filesystems are case-insensitive, so containment comparisons must be too).
type Root struct {
	abs string
	win bool
}

// NewRoot validates and locks in the project root. The directory must exist;
// EvalSymlinks resolves any symlink chain in it, and the resolved absolute
// path becomes the containment boundary. This happens ONCE at startup so the
// root itself can never be swapped for a symlink later.
func NewRoot(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("project root %q: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("project root %q is not a directory", abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve project root %q: %w", abs, err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return nil, err
	}
	return &Root{abs: filepath.Clean(real), win: runtime.GOOS == "windows"}, nil
}

// Abs returns the canonical absolute path of the project root.
func (r *Root) Abs() string {
	return r.abs
}

// inside reports whether the absolute path candidate is at or below the
// root. On Windows the comparison is case-insensitive (EqualFold semantics)
// because the filesystem itself is case-insensitive; on Unix it is exact.
func (r *Root) inside(candidate string) bool {
	if r.win {
		lc := strings.ToLower(candidate)
		root := strings.ToLower(r.abs)
		if lc == root {
			return true
		}
		return strings.HasPrefix(lc, root+`\`)
	}
	if candidate == r.abs {
		return true
	}
	return strings.HasPrefix(candidate, r.abs+string(os.PathSeparator))
}

// canonical is the pure lexical containment check: candidate is already
// absolute and cleaned; this returns it unchanged if it is inside the root
// (mirroring inside's case rules) or ErrOutside otherwise.
func (r *Root) canonical(candidate string) (string, error) {
	if r.win {
		lc := strings.ToLower(candidate)
		root := strings.ToLower(r.abs)
		if lc == root {
			return r.abs, nil
		}
		if strings.HasPrefix(lc, root+`\`) {
			return r.abs + candidate[len(r.abs):], nil
		}
		return "", ErrOutside
	}
	rel, err := filepath.Rel(r.abs, candidate)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", ErrOutside
	}
	return candidate, nil
}

// elementStem strips everything from the first "." of a path element, so
// "NUL.txt" and "CON.log" reduce to "NUL"/"CON" for the device-name check.
func elementStem(elem string) string {
	if i := strings.Index(elem, "."); i >= 0 {
		return elem[:i]
	}
	return elem
}

// Resolve is the single security gate every file operation must pass. It
// turns a model-supplied path (relative to the root, or absolute) into an
// absolute path inside the root, rejecting anything suspicious. The checks,
// in order:
//  1. NUL bytes - invalid in paths and a classic injection trick;
//  2. "/" normalized to "\" on Windows so both separators behave the same;
//  3. the "\\?\" / "\\.\" prefixes (they disable normal path semantics);
//  4. absolute paths kept, relative paths joined onto the root, then Clean;
//  5. Windows-only: UNC prefixes, drive-relative segments, ":" (alternate
//     data streams) and reserved device names anywhere in the path;
//  6. canonical: the result must still sit inside the root;
//  7. the first path element must not be ".agent" (harness-owned state);
//  8. every existing symlink/junction along the path must resolve to a
//     target inside the root (see checkSymlinks).
func (r *Root) Resolve(requested string) (string, error) {
	if strings.IndexByte(requested, 0) >= 0 {
		return "", ErrOutside
	}
	p := requested
	if r.win {
		p = strings.ReplaceAll(p, "/", `\`)
	}
	if filepath.IsAbs(p) {
		if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
			return "", ErrDevice
		}
	} else {
		p = filepath.Join(r.abs, p)
	}
	p = filepath.Clean(p)

	if r.win {
		if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, `//`) {
			return "", ErrDevice
		}
		volume := filepath.VolumeName(p)
		rest := strings.TrimPrefix(p, volume)
		rest = strings.TrimPrefix(rest, `\`)
		for _, elem := range strings.Split(rest, `\`) {
			if elem == "" {
				continue
			}
			if strings.Contains(elem, ":") {
				return "", ErrDevice
			}
			if reservedDeviceNames[strings.ToUpper(elementStem(elem))] {
				return "", ErrDevice
			}
		}
	}

	p, err := r.canonical(p)
	if err != nil {
		return "", err
	}

	// Reject anything whose first element is .agent, case-insensitively.
	rel, err := filepath.Rel(r.abs, p)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	if rel != "" {
		first := rel
		if i := strings.IndexByte(first, '/'); i >= 0 {
			first = first[:i]
		}
		if strings.EqualFold(first, ".agent") {
			return "", ErrProtected
		}
	}

	if err := r.checkSymlinks(p); err != nil {
		return "", err
	}
	return p, nil
}

// checkSymlinks walks the resolved path element by element from the root
// down and inspects each EXISTING directory with Lstat. Lstat (unlike Stat)
// does not follow symlinks, so it reveals them; EvalSymlinks then resolves
// the link and we verify the target is still inside the root. Two failure
// modes are caught: a link pointing outside the root, and a dangling link
// (which must be rejected - otherwise a later write could create the target
// elsewhere and let a file escape through a now-resolvable link). A link
// resolving into .agent is likewise refused. On Windows, NTFS junctions
// surface as symlinks too, so this closes the classic junction escape.
func (r *Root) checkSymlinks(p string) error {
	// Split the path into its elements relative to the root, then walk them
	// one by one (cur grows root -> child -> ...). Windows paths must be
	// matched case-insensitively, hence the explicit branch; the root itself
	// has no elements and needs no walk.
	var cur string
	segs := []string{}
	if r.win {
		lc := strings.ToLower(p)
		root := strings.ToLower(r.abs)
		if lc == root {
			cur = r.abs
		} else {
			cur = r.abs
			segs = strings.Split(p[len(r.abs)+1:], `\`)
		}
	} else {
		rel, err := filepath.Rel(r.abs, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		cur = r.abs
		segs = strings.Split(rel, string(os.PathSeparator))
	}
	for _, seg := range segs {
		cur = filepath.Join(cur, seg)
		st, err := os.Lstat(cur)
		if err != nil {
			// A missing element simply ends the walk: nothing below it
			// exists yet, so there is nothing that could be a symlink.
			return nil
		}
		if st.Mode()&os.ModeSymlink == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(cur)
		if err != nil {
			// Dangling symlink (or broken chain): reject outright - see the
			// function comment for why this is the safe choice.
			return fmt.Errorf("%w: %v", ErrSymlink, err)
		}
		if !r.inside(real) {
			return fmt.Errorf("%w: %q resolves to %q", ErrOutside, cur, real)
		}
		// A symlink inside the root may still point INTO the protected
		// state dir (root/.agent via a link elsewhere) - check and refuse.
		if rel, err := filepath.Rel(r.abs, real); err == nil {
			rel = filepath.ToSlash(rel)
			first := rel
			if i := strings.IndexByte(first, '/'); i >= 0 {
				first = first[:i]
			}
			if strings.EqualFold(first, ".agent") {
				return fmt.Errorf("%w: %q resolves into the protected state dir", ErrProtected, cur)
			}
		}
		// Continue the walk from the link target, so a chain of links
		// (a -> b -> ...) is each verified in turn.
		cur = real
	}
	return nil
}

// DisplayRel renders an absolute path inside the root as a slash-separated
// relative path for the model's listings ("." for the root itself).
func DisplayRel(root *Root, abs string) string {
	rel, err := filepath.Rel(root.abs, abs)
	if err != nil {
		return abs
	}
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}
