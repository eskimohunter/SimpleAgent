package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRoot(t *testing.T) *Root {
	t.Helper()
	r, err := NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveRejectsEscapes(t *testing.T) {
	r := testRoot(t)
	cases := []string{
		"..",
		"../x",
		"a/../../x",
		"../../etc/passwd",
		"a/b/../../../../..",
	}
	if abs := r.Abs(); filepath.IsAbs(abs) {
		// absolute paths outside root are rejected everywhere
		other := filepath.Dir(abs)
		cases = append(cases, other)
		cases = append(cases, other+string(os.PathSeparator)+"x")
	}
	for _, c := range cases {
		if _, err := r.Resolve(c); err == nil {
			t.Errorf("Resolve(%q) unexpectedly succeeded", c)
		}
	}
}

func TestResolveAllowsInside(t *testing.T) {
	r := testRoot(t)
	cases := []string{"", ".", "a", "a/b", "a/../b", "./x", "deep/nested/file.txt"}
	for _, c := range cases {
		if _, err := r.Resolve(c); err != nil {
			t.Errorf("Resolve(%q): %v", c, err)
		}
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("evil/secret.txt"); err == nil {
		t.Fatal("read through escaping symlink must be rejected")
	}
	if _, err := r.Resolve("evil/newfile"); err == nil {
		t.Fatal("write through escaping symlink must be rejected")
	}
}

func TestDanglingSymlinkWriteRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, "dangle")
	target := filepath.Join(outside, "not-yet-created")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("dangle"); err == nil {
		t.Fatal("dangling symlink must be rejected (write escape vector)")
	}
}

func TestSymlinkInsideAllowed(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSandbox(r, FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	if _, err := s.WriteFile("alias/f.txt", "inside", false); err != nil {
		t.Fatalf("internal symlink write should work: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realDir, "f.txt")); err != nil {
		t.Fatalf("file should land in the real directory: %v", err)
	}
	out, err := s.ReadFile("alias/f.txt", 0, 0)
	if err != nil || !strings.Contains(out, "inside") {
		t.Fatalf("read back through symlink failed: %v %q", err, out)
	}
}

func TestAgentStateProtected(t *testing.T) {
	r := testRoot(t)
	for _, c := range []string{".agent", ".agent/approvals.json", ".agent/x/y"} {
		if _, err := r.Resolve(c); err == nil {
			t.Errorf("Resolve(%q) should be blocked", c)
		}
	}
	p, err := r.Resolve("my.agent/file")
	if err != nil {
		t.Errorf("directory named my.agent is fine: %v", err)
	}
	_ = p
}

func TestSymlinkIntoAgentStateRejected(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".agent")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "audit.jsonl"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(stateDir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("alias/audit.jsonl"); err == nil {
		t.Fatal("symlink resolving into .agent must be rejected")
	}
	if _, err := r.Resolve("alias"); err == nil {
		t.Fatal("symlink whose target IS .agent must be rejected")
	}
}

func TestRootItselfAllowed(t *testing.T) {
	r := testRoot(t)
	p, err := r.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if p != r.Abs() {
		t.Errorf("expected %q got %q", r.Abs(), p)
	}
}

func TestProtectedFileBlocked(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "simpleagent.json")
	if err := os.WriteFile(cfgPath, []byte(`{"model":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Protect(cfgPath)
	if _, err := r.Resolve("simpleagent.json"); err == nil {
		t.Fatal("protected config file must be blocked")
	}
	// Any access form is blocked, and an unrelated file stays reachable.
	if _, err := r.Resolve("./simpleagent.json"); err == nil {
		t.Fatal("normalized alias of protected file must be blocked")
	}
	if p, err := r.Resolve("other.txt"); err != nil {
		t.Fatalf("unrelated file must stay resolvable: %v", err)
	} else {
		_ = p
	}
}

func TestProtectedNonexistentCreationBlocked(t *testing.T) {
	// A protected file need not exist: the agent must not be able to create
	// <root>/simpleagent.json for the next launch either.
	r := testRoot(t)
	r.Protect(filepath.Join(r.Abs(), "simpleagent.json"))
	if _, err := r.Resolve("simpleagent.json"); err == nil {
		t.Fatal("creating a protected path must be blocked before touching disk")
	}
}

func TestProtectedFileSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "simpleagent.json")
	if err := os.WriteFile(cfgPath, []byte(`{"model":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias.txt")
	if err := os.Symlink(cfgPath, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r, err := NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Protect(cfgPath)
	if _, err := r.Resolve("alias.txt"); err == nil {
		t.Fatal("write/read through a symlink to the protected config must be rejected")
	}
}

func TestProtectedFileCaseInsensitive(t *testing.T) {
	// Case folding is unconditional: Windows and macOS filesystems are
	// case-insensitive by default, so SIMPLEAGENT.JSON must not reach the
	// real config. On case-sensitive filesystems this over-protects files
	// differing only by case, which is the safe direction (same tradeoff
	// the .agent checks already make).
	r := testRoot(t)
	r.Protect(filepath.Join(r.Abs(), "simpleagent.json"))
	if _, err := r.Resolve("SimpleAgent.JSON"); err == nil {
		t.Fatal("case variant of a protected file must be blocked")
	}
}

func TestProtectCanonicalizesSymlinkedPath(t *testing.T) {
	// Protect must match Resolve output even when the registered path runs
	// through a symlink (e.g. --config under a symlinked directory), and
	// even when the protected file does not exist yet.
	rootDir := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "proj-link")
	if err := os.Symlink(rootDir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	r, err := NewRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	r.Protect(filepath.Join(link, "custom.json"))
	if _, err := r.Resolve("custom.json"); err == nil {
		t.Fatal("protected path registered through a symlink must match Resolve")
	}
	if _, err := r.Resolve("other.json"); err != nil {
		t.Fatalf("unrelated file must stay resolvable: %v", err)
	}
}

func TestDisplayRel(t *testing.T) {
	r := testRoot(t)
	p := filepath.Join(r.Abs(), "src", "a.go")
	if got := DisplayRel(r, p); got != "src/a.go" {
		t.Errorf("got %q", got)
	}
	if got := DisplayRel(r, r.Abs()); got != "." {
		t.Errorf("got %q", got)
	}
}
