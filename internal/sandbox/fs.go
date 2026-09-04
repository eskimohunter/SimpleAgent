package sandbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// FilesConfig carries the byte caps applied to every file read and write.
type FilesConfig struct {
	MaxReadBytes  int
	MaxWriteBytes int
}

// Sandbox bundles the confinement Root with the size caps, and implements
// the four file tools the model may call: List, ReadFile, WriteFile and
// Search. Every method starts by routing its path argument through
// Root.Resolve - so nothing here can touch a file outside the root, no
// matter what the caller (the model) asked for.
type Sandbox struct {
	root *Root
	cfg  FilesConfig
}

// Entry is one row of a directory listing.
type Entry struct {
	Path  string
	Size  int64
	IsDir bool
}

// NewFiles builds a Sandbox rooted at the current working directory. Used
// by tests that want a sandbox without a pre-made Root.
func NewFiles(cfg FilesConfig) (*Sandbox, error) {
	r, err := NewRoot(".")
	if err != nil {
		return nil, err
	}
	return &Sandbox{root: r, cfg: cfg}, nil
}

// NewSandbox builds a Sandbox around an existing Root (the normal startup
// path in main.go).
func NewSandbox(root *Root, cfg FilesConfig) *Sandbox {
	return &Sandbox{root: root, cfg: cfg}
}

// Root exposes the containment root so callers can get the project's
// absolute path (e.g. as the working directory for shell commands).
func (s *Sandbox) Root() *Root {
	return s.root
}

// human renders a byte count compactly for listings (1 KB, 2.5 MB, ...).
func human(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

// List renders a directory listing as text for the model: one level, or
// recursively (depth-capped at 8, entry-capped at maxEntries, 2000 by
// default) so a huge tree cannot flood the context window. ".agent" is
// shown as "protected" in flat listings and skipped entirely in recursive
// ones.
func (s *Sandbox) List(path string, recursive bool, maxEntries int) (string, error) {
	if maxEntries <= 0 {
		maxEntries = 2000
	}
	abs, err := s.root.Resolve(path)
	if err != nil {
		return "", err
	}
	relBase := DisplayRel(s.root, abs)
	if relBase == "." {
		relBase = ""
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", disp(relBase))
	}
	if recursive {
		return s.listRecursive(abs, relBase, maxEntries)
	}
	return s.listFlat(abs, relBase)
}

// isHiddenState reports whether a directory entry is the harness state dir
// (case-insensitive, for Windows).
func isHiddenState(name string) bool {
	return strings.EqualFold(name, ".agent")
}

// disp renders an empty relative path as "." for user-facing messages.
func disp(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// listFlat renders one directory level, sorted by name, with sizes.
func (s *Sandbox) listFlat(dirAbs, relBase string) (string, error) {
	ents, err := os.ReadDir(dirAbs)
	if err != nil {
		return "", err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var b strings.Builder
	b.WriteString(fmt.Sprintf("listing %s (%d entries)\n", disp(relBase), len(ents)))
	for _, e := range ents {
		if isHiddenState(e.Name()) {
			b.WriteString("dir  " + joinSlash(relBase, e.Name()) + "/   (harness state, protected)\n")
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		b.WriteString(fmt.Sprintf("%s  %-40s %10s\n", kind(fi), joinSlash(relBase, e.Name()), human(fi.Size())))
	}
	return b.String(), nil
}

// listRecursive walks the tree with a recursive closure (a function value
// that calls itself - note it must be declared with var first so the closure
// can reference itself). Depth and total entries are capped; errStopListing
// is the sentinel that aborts the walk early without being an error.
func (s *Sandbox) listRecursive(dirAbs, relBase string, maxEntries int) (string, error) {
	var b strings.Builder
	count := 0
	entries := []Entry{}
	var walk func(dirAbs, rel string, depth int) error
	walk = func(dirAbs, rel string, depth int) error {
		if depth > 8 {
			return nil
		}
		ents, err := os.ReadDir(dirAbs)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if isHiddenState(e.Name()) {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			if count >= maxEntries {
				return errStopListing
			}
			relPath := joinSlash(rel, e.Name())
			entries = append(entries, Entry{Path: relPath, Size: fi.Size(), IsDir: fi.IsDir()})
			count++
			if fi.IsDir() {
				if err := walk(filepath.Join(dirAbs, e.Name()), relPath, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := walk(dirAbs, relBase, 0)
	if err != nil && !errors.Is(err, errStopListing) {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	truncated := false
	if len(entries) >= maxEntries {
		truncated = true
		entries = entries[:maxEntries]
	}
	b.WriteString(fmt.Sprintf("recursive listing of %s (%d entries", disp(relBase), len(entries)))
	if truncated {
		b.WriteString(", truncated")
	}
	b.WriteString(")\n")
	for _, e := range entries {
		name := e.Path
		if e.IsDir {
			b.WriteString(fmt.Sprintf("dir  %-40s\n", name+"/"))
		} else {
			b.WriteString(fmt.Sprintf("file %-40s %10s\n", name, human(e.Size)))
		}
	}
	if truncated {
		b.WriteString("... more entries omitted (increase limit or narrow the path)\n")
	}
	return b.String(), nil
}

// errStopListing is a sentinel error used internally to halt a recursive
// listing once the entry cap is reached; it is filtered out of results, so
// callers never see it (errors.Is distinguishes it from real failures).
var errStopListing = errors.New("listing stopped")

// joinSlash concatenates a relative base and a name with "/" (the model
// always sees forward slashes, even on Windows).
func joinSlash(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

// kind returns the one-letter-ish kind tag used in flat listings.
func kind(fi fs.FileInfo) string {
	if fi.IsDir() {
		return "dir "
	}
	return "file"
}

type lineNumberer struct {
	width int
}

// ReadFile implements the read_file tool. Small files (at or under the read
// cap, no offset given) are read whole; larger files must be paged with a
// 1-based line offset so reads stay within the output cap. Returns text
// with "N|" line-number prefixes (see readWhole/readRanged).
func (s *Sandbox) ReadFile(path string, offset, limit int) (string, error) {
	abs, err := s.root.Resolve(path)
	if err != nil {
		return "", err
	}
	rel := DisplayRel(s.root, abs)
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", disp(rel))
	}

	if offset <= 0 && fi.Size() <= int64(s.cfg.MaxReadBytes) {
		return s.readWhole(rel, abs, fi.Size())
	}
	if offset <= 0 {
		return "", fmt.Errorf("file is %s (%d bytes), larger than the %s read cap; pass offset/limit line numbers instead",
			disp(rel), fi.Size(), human(int64(s.cfg.MaxReadBytes)))
	}
	if limit <= 0 {
		limit = 500
	}
	return s.readRanged(rel, abs, offset, limit)
}

// numWidth returns the column width needed to print count line numbers
// (grows from 4 as the number of digits grows; overkill for huge files but
// keeps the ruler stable within one listing).
func numWidth(count int) int {
	w := 4
	for n := count; n >= 10; n /= 10 {
		w++
	}
	return w
}

// readWhole reads an entire small file and renders it line-numbered. Binary
// junk is sanitized: invalid UTF-8 bytes are replaced with U+FFFD (the
// replacement character) so the model never receives corrupt text.
func (s *Sandbox) readWhole(rel, abs string, size int64) (string, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.ToValidUTF8(string(data), "\uFFFD"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	w := numWidth(len(lines))
	b.WriteString(fmt.Sprintf("# %s: %d lines, %s\n", disp(rel), len(lines), human(size)))
	for i, ln := range lines {
		fmt.Fprintf(&b, "%*d| %s\n", w, i+1, ln)
	}
	return b.String(), nil
}

// readRanged pages through a file starting at 1-based line offset. It scans
// (never loads the whole file) and enforces several safety limits: the
// scanner buffer is capped at MaxReadBytes per line, single lines are
// truncated at 4000 characters, and the total output stops once it reaches
// the read cap - with a "# ... more lines follow" hint telling the model how
// to continue.
func (s *Sandbox) readRanged(rel, abs string, offset, limit int) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if offset < 1 {
		offset = 1
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), s.cfg.MaxReadBytes)
	cur := 0
	var b strings.Builder
	w := 6
	shown := 0
	more := false
	for sc.Scan() {
		cur++
		if cur < offset {
			continue
		}
		if shown >= limit {
			more = true
			break
		}
		line := strings.ToValidUTF8(sc.Text(), "\uFFFD")
		if len(line) > 4000 {
			line = line[:4000] + "..."
		}
		fmt.Fprintf(&b, "%*d| %s\n", w, cur, line)
		shown++
		if b.Len() >= s.cfg.MaxReadBytes {
			more = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read %s: %v", disp(rel), err)
	}
	var out strings.Builder
	if shown == 0 {
		out.WriteString(fmt.Sprintf("# %s: no lines to show from offset %d (file is %s); the file has fewer lines\n", disp(rel), offset, human(fileSize(abs))))
		return out.String(), nil
	}
	out.WriteString(fmt.Sprintf("# %s: showing lines %d-%d (of size %s)\n", disp(rel), offset, offset+shown-1, human(fileSize(abs))))
	out.WriteString(b.String())
	if more {
		out.WriteString("# ... more lines follow; pass offset=" + fmt.Sprint(offset+shown) + " to continue\n")
	}
	return out.String(), nil
}

// fileSize stats a path and returns its size (0 on any error). Used only
// for display in readRanged's messages, so errors are ignored.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// WriteFile implements the write_file tool: it overwrites (or, with append,
// extends) the file at path with content. Parents are created as needed.
// The content cap is checked BEFORE any filesystem work, so an oversized
// write never touches disk.
func (s *Sandbox) WriteFile(path, content string, append_ bool) (string, error) {
	if len(content) > s.cfg.MaxWriteBytes {
		return "", fmt.Errorf("content is %d bytes, above the %d byte write cap", len(content), s.cfg.MaxWriteBytes)
	}
	abs, err := s.root.Resolve(path)
	if err != nil {
		return "", err
	}
	rel := DisplayRel(s.root, abs)
	fi, err := os.Lstat(abs)
	if err == nil && fi.IsDir() {
		return "", fmt.Errorf("%s is a directory", disp(rel))
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	// append_ is named with a trailing underscore because "append" is a
	// builtin function in Go and cannot be used as an identifier.
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	verb := "wrote"
	if append_ {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
		verb = "appended"
	}
	f, err := os.OpenFile(abs, flags, 0o644)
	if err != nil {
		return "", err
	}
	n, werr := f.WriteString(content)
	cerr := f.Close()
	if werr != nil {
		return "", werr
	}
	if cerr != nil {
		return "", cerr
	}
	lines := 0
	if content != "" {
		lines = bytes.Count([]byte(content), []byte("\n"))
		if !strings.HasSuffix(content, "\n") {
			lines++
		}
	}
	return fmt.Sprintf("%s %d bytes (%d lines) to %s", verb, n, lines, disp(rel)), nil
}

// Search implements the search_files tool: a contained regex grep over the
// project tree (Go regexp syntax). Defensive limits keep a hostile or
// hallucinated query cheap: at most 10,000 files are scanned, files over
// 32 MB are skipped, binary files are detected (a NUL byte in the first 8 KB
// means skip - text files almost never contain NUL), and each file reports
// at most 50 lines, 500 matches globally. Symlinks, .agent and .git are
// never followed or searched.
func (s *Sandbox) Search(pattern, include string, maxResults int) (string, error) {
	if maxResults <= 0 {
		maxResults = 500
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %v", err)
	}
	var incGlob *string
	if include != "" {
		incGlob = &include
	}
	var b strings.Builder
	matches := 0
	scanned := 0
	// matched collects one hit into the output builder, capped globally.
	matched := func(rel, line string, lineno int) {
		if matches >= maxResults {
			return
		}
		ln := strings.ToValidUTF8(line, "\uFFFD")
		if len(ln) > 400 {
			ln = ln[:400] + "..."
		}
		fmt.Fprintf(&b, "%s:%d: %s\n", rel, lineno, ln)
		matches++
	}
	err = filepath.WalkDir(s.root.abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable directory etc.: skip rather than fail the search.
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// WalkDir does not follow symlinks, but listing them could
			// confuse the results; more importantly it never descends into
			// a linked directory, so no link target outside the root is
			// ever scanned.
			return nil
		}
		name := d.Name()
		if isHiddenState(name) || strings.EqualFold(name, ".git") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if incGlob != nil {
			if !matchGlob(*incGlob, name, p) {
				return nil
			}
		}
		if scanned >= 10000 {
			// filepath.SkipAll ends the whole walk, not just this branch.
			return filepath.SkipAll
		}
		scanned++
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.Size() > 32*1024*1024 {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		// Binary sniff: read the first 8 KB; a NUL byte means "not text".
		head := make([]byte, 8192)
		n, _ := f.Read(head)
		if bytes.IndexByte(head[:n], 0) >= 0 {
			return nil
		}
		if _, err := f.Seek(0, 0); err != nil {
			return nil
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		lineNo := 0
		fileHits := 0
		for sc.Scan() {
			lineNo++
			if re.Match(sc.Bytes()) {
				matched(DisplayRel(s.root, p), sc.Text(), lineNo)
				fileHits++
				if fileHits >= 50 || matches >= maxResults {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if matches == 0 {
		return fmt.Sprintf("no matches for pattern %q%s", pattern, includeNote(include)), nil
	}
	out := fmt.Sprintf("# %d match(es)", matches)
	if matches >= maxResults {
		out += " (capped)"
	}
	out += fmt.Sprintf(" for pattern %q%s\n%s", pattern, includeNote(include), b.String())
	return out, nil
}

// includeNote words the "where" part of the search summary for the model.
func includeNote(include string) string {
	if include == "" {
		return " in any file"
	}
	return " in files matching " + include
}

// matchGlob matches a glob against both the file's base name and its full
// slash-form path, so include patterns like "*.go" and "test/**" both work.
func matchGlob(glob, base, path string) bool {
	ok, err := filepath.Match(glob, base)
	if err == nil && ok {
		return true
	}
	ok, err = filepath.Match(glob, filepath.ToSlash(path))
	if err == nil && ok {
		return true
	}
	return false
}
