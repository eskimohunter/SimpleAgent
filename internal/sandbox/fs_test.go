package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestSandbox(t *testing.T) *Sandbox {
	t.Helper()
	root := testRoot(t)
	s := NewSandbox(root, FilesConfig{MaxReadBytes: 1024, MaxWriteBytes: 1 << 20})
	return s
}

func write(t *testing.T, s *Sandbox, path, content string) {
	t.Helper()
	_, err := s.WriteFile(path, content, false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteReadRoundtrip(t *testing.T) {
	s := newTestSandbox(t)
	content := "hello\nworld\n"
	write(t, s, "sub/dir/a.txt", content)
	out, err := s.ReadFile("sub/dir/a.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Errorf("content missing: %q", out)
	}
	if !strings.Contains(out, "2 lines") {
		t.Errorf("expected line count header, got %q", out)
	}
}

func TestAppend(t *testing.T) {
	s := newTestSandbox(t)
	write(t, s, "f.txt", "one\n")
	_, err := s.WriteFile("f.txt", "two\n", true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.Root().Abs(), "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("got %q", string(data))
	}
}

func TestReadRange(t *testing.T) {
	s := newTestSandbox(t)
	var sb strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&sb, "line %06d %s\n", i, strings.Repeat("x", 60))
	}
	write(t, s, "big.txt", sb.String())
	out, err := s.ReadFile("big.txt", 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "line 000100") {
		t.Errorf("expected offset line, got %q", out)
	}
	if strings.Contains(out, "line 000099") {
		t.Errorf("must not include earlier lines")
	}
	if !strings.Contains(out, "more lines follow") {
		t.Errorf("expected continuation hint, got %q", out)
	}
}

func TestReadOffsetPastEOF(t *testing.T) {
	s := newTestSandbox(t)
	write(t, s, "small.txt", "one\ntwo\nthree\n")
	out, err := s.ReadFile("small.txt", 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no lines to show from offset 100") {
		t.Errorf("expected past-EOF notice, got %q", out)
	}
	if strings.Contains(out, "showing lines") {
		t.Errorf("must not print a bogus range: %q", out)
	}
}

func TestReadTooLargeRequiresRange(t *testing.T) {
	s := newTestSandbox(t)
	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&sb, "row %03d %s\n", i, strings.Repeat("y", 60))
	}
	write(t, s, "huge.txt", sb.String())
	_, err := s.ReadFile("huge.txt", 0, 0)
	if err == nil {
		t.Fatal("expected size error without offset")
	}
	out, err := s.ReadFile("huge.txt", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "showing lines 1-10") || !strings.Contains(out, "row 001") {
		t.Errorf("unexpected header/content: %q", out)
	}
	if strings.Contains(out, "row 011") {
		t.Errorf("must stop after limit lines")
	}
}

func TestWriteOutsideRejected(t *testing.T) {
	s := newTestSandbox(t)
	outside := filepath.Join(s.Root().Abs(), "..", "escapeme.txt")
	if _, err := s.WriteFile(outside, "x", false); err == nil {
		t.Fatal("absolute path outside root must be rejected")
	}
	if _, err := s.WriteFile("../../esc2.txt", "x", false); err == nil {
		t.Fatal(".. traversal must be rejected")
	}
	if _, err := s.WriteFile(".agent/evil.json", "x", false); err == nil {
		t.Fatal(".agent write must be rejected")
	}
	if _, err := s.ReadFile(".agent/approvals.json", 0, 0); err == nil {
		t.Fatal(".agent read must be rejected")
	}
}

func TestWriteCap(t *testing.T) {
	s := newTestSandbox(t)
	if _, err := s.WriteFile("big.bin", strings.Repeat("z", 2<<20), false); err == nil {
		t.Fatal("write above cap must fail")
	}
}

func TestSearch(t *testing.T) {
	s := newTestSandbox(t)
	write(t, s, "a.go", "package main\n// TODO: fix\nfunc main() {}\n")
	write(t, s, "b.txt", "nothing here\nTODO later\n")
	agentDir := filepath.Join(s.Root().Abs(), ".agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "secret.txt"), []byte("TODO hidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := s.Search("TODO", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".agent") {
		t.Errorf("search must skip .agent: %q", out)
	}
	if !strings.Contains(out, "a.go:2") || !strings.Contains(out, "b.txt:2") {
		t.Errorf("expected matches in both files: %q", out)
	}
	out, err = s.Search("TODO", "*.go", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "b.txt") {
		t.Errorf("include glob not honored: %q", out)
	}
}

func TestSearchBinarySkipped(t *testing.T) {
	s := newTestSandbox(t)
	write(t, s, "bin.dat", "a\x00b\x00c")
	out, err := s.Search("a", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "bin.dat") {
		t.Errorf("binary file should be skipped: %q", out)
	}
}

func TestSearchSkipsSymlinks(t *testing.T) {
	s := newTestSandbox(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.Root().Abs(), "leak.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	out, err := s.Search("TOP-SECRET-MARKER", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "leak.txt:") {
		t.Errorf("search must not follow symlinks outside the root: %q", out)
	}
}

func TestListHidesAgent(t *testing.T) {
	s := newTestSandbox(t)
	write(t, s, "one.txt", "1")
	out, err := s.List("", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "one.txt") {
		t.Errorf("expected one.txt: %q", out)
	}
	if strings.Contains(out, ".agent/approvals.json") {
		t.Errorf("list must not expose .agent contents: %q", out)
	}
}

func TestListRecursive(t *testing.T) {
	s := newTestSandbox(t)
	write(t, s, "x/y/z.txt", "z")
	out, err := s.List("", true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "x/y/z.txt") {
		t.Errorf("recursive listing missing nested file: %q", out)
	}
}

func TestProtectedConfigSurface(t *testing.T) {
	s := newTestSandbox(t)
	cfgPath := filepath.Join(s.Root().Abs(), "simpleagent.json")
	if err := os.WriteFile(cfgPath, []byte(`{"base_url": "SECRET-URL-MARKER"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Root().Protect(cfgPath)
	write(t, s, "other.txt", "SECRET-URL-MARKER\n")

	// Read and write through the sandbox tools are refused outright.
	if _, err := s.ReadFile("simpleagent.json", 0, 0); err == nil {
		t.Fatal("read of protected config must be rejected")
	}
	if _, err := s.WriteFile("simpleagent.json", "pwned", false); err == nil {
		t.Fatal("write to protected config must be rejected")
	}

	// Flat listing shows the tag, not file contents.
	flat, err := s.List("", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(flat, "harness config, protected") {
		t.Errorf("flat listing must tag the protected config: %q", flat)
	}

	// Recursive listing omits it, search never reads it.
	rec, err := s.List("", true, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec, "simpleagent.json") {
		t.Errorf("recursive listing must omit the protected config: %q", rec)
	}
	out, err := s.Search("SECRET-URL-MARKER", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "simpleagent.json:") {
		t.Errorf("search must skip the protected config: %q", out)
	}
	if !strings.Contains(out, "other.txt:") {
		t.Errorf("search must still find unprotected files: %q", out)
	}
}
