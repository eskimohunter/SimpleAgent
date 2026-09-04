package approvals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMatching(t *testing.T) {
	m, err := New("", true, []string{"git status", "git diff*", "go test*"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cmd string
		ok  bool
		src string
	}{
		{"git status", true, "exact"},
		{"  git   status  ", true, "exact"},
		{"git diff HEAD", true, "prefix"},
		{"go test ./...", true, "prefix"},
		{"git push origin main", false, ""},
		{"rm -rf /", false, ""},
		{"go test", true, "prefix"},
	}
	for _, c := range cases {
		ok, src := m.Allowed(c.cmd)
		if ok != c.ok || src != c.src {
			t.Errorf("Allowed(%q) = (%v,%q), want (%v,%q)", c.cmd, ok, src, c.ok, c.src)
		}
	}
}

func TestRememberPersists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "approvals.json")
	m, err := New(file, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remember("echo hello"); err != nil {
		t.Fatal(err)
	}
	m2, err := New(file, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	ok, src := m2.Allowed("echo hello")
	if !ok || src != "exact" {
		t.Fatalf("persisted approval missing: %v %q", ok, src)
	}
	data, _ := os.ReadFile(file)
	var st store
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Commands) != 1 || st.Commands[0] != "echo hello" {
		t.Fatalf("store content: %+v", st)
	}
}

func TestRememberNoPersist(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "approvals.json")
	m, err := New(file, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Remember("echo x")
	if _, err := os.Stat(file); err == nil {
		t.Fatal("persist disabled must not write the store file")
	}
	ok, _ := m.Allowed("echo x")
	if !ok {
		t.Fatal("in-memory approval should hold for the session")
	}
}

func TestCorruptStore(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "approvals.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(file, true, nil); err == nil {
		t.Fatal("corrupt store must error")
	}
}

func TestDedupeAndList(t *testing.T) {
	m, err := New("", true, []string{"b", "a", "z*"})
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Remember("b")
	_ = m.Remember("b")
	_ = m.Remember("c")
	exact, prefixes := m.List()
	if len(exact) != 3 {
		t.Fatalf("expected 3 exact, got %v", exact)
	}
	if exact[0] != "a" || exact[2] != "c" {
		t.Fatalf("sorted expected, got %v", exact)
	}
	if len(prefixes) != 1 || prefixes[0] != "z" {
		t.Fatalf("prefixes: %v", prefixes)
	}
}
