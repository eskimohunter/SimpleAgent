package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	ErrOutside   = errors.New("path escapes project root")
	ErrProtected = errors.New("path refers to harness state (.agent), which is protected")
	ErrDevice    = errors.New("path contains a Windows device name or alternate stream")
	ErrSymlink   = errors.New("path contains an unresolvable or escaping symlink")
)

var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

type Root struct {
	abs string
	win bool
}

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

func (r *Root) Abs() string {
	return r.abs
}

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

func elementStem(elem string) string {
	if i := strings.Index(elem, "."); i >= 0 {
		return elem[:i]
	}
	return elem
}

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

func (r *Root) checkSymlinks(p string) error {
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
			return nil
		}
		if st.Mode()&os.ModeSymlink == 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(cur)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrSymlink, err)
		}
		if !r.inside(real) {
			return fmt.Errorf("%w: %q resolves to %q", ErrOutside, cur, real)
		}
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
		cur = real
	}
	return nil
}

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
