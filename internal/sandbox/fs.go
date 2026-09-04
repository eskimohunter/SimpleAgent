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

type FilesConfig struct {
	MaxReadBytes  int
	MaxWriteBytes int
}

type Sandbox struct {
	root *Root
	cfg  FilesConfig
}

type Entry struct {
	Path  string
	Size  int64
	IsDir bool
}

func NewFiles(cfg FilesConfig) (*Sandbox, error) {
	r, err := NewRoot(".")
	if err != nil {
		return nil, err
	}
	return &Sandbox{root: r, cfg: cfg}, nil
}

func NewSandbox(root *Root, cfg FilesConfig) *Sandbox {
	return &Sandbox{root: root, cfg: cfg}
}

func (s *Sandbox) Root() *Root {
	return s.root
}

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

func isHiddenState(name string) bool {
	return strings.EqualFold(name, ".agent")
}

func disp(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

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

var errStopListing = errors.New("listing stopped")

func joinSlash(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

func kind(fi fs.FileInfo) string {
	if fi.IsDir() {
		return "dir "
	}
	return "file"
}

type lineNumberer struct {
	width int
}

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

func numWidth(count int) int {
	w := 4
	for n := count; n >= 10; n /= 10 {
		w++
	}
	return w
}

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

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

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
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
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

func includeNote(include string) string {
	if include == "" {
		return " in any file"
	}
	return " in files matching " + include
}

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
