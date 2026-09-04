package approvals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMatching(t *testing.T) {
	m, err := New("", true, []string{"git status", "git diff*", "go test*"}, nil)
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
	m, err := New(file, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remember("echo hello"); err != nil {
		t.Fatal(err)
	}
	m2, err := New(file, true, nil, nil)
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
	m, err := New(file, false, nil, nil)
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
	if _, err := New(file, true, nil, nil); err == nil {
		t.Fatal("corrupt store must error")
	}
}

func TestDedupeAndList(t *testing.T) {
	m, err := New("", true, []string{"b", "a", "z*"}, nil)
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

func TestDenyMatching(t *testing.T) {
	m, err := New("", true, []string{"git status"}, []string{"curl*", "rm -rf /", "wget*", "Invoke-WebRequest*"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cmd string
		ok  bool
		src string
	}{
		{"curl https://example.com/x", true, "prefix"},
		{"  curl   -o out https://x  ", true, "prefix"},
		{"curl.exe /L x", true, "prefix"},
		{"rm -rf /", true, "exact"},
		{"wget --quiet https://x", true, "prefix"},
		{"Invoke-WebRequest -Uri http://x", true, "prefix"},
		// The deny side folds case (PowerShell is case-insensitive).
		{"CURL https://example.com/x", true, "prefix"},
		{"RM -RF /", true, "exact"},
		// No match: unrelated commands pass through, deny does not collide
		// with the allow side, and prefixes do not match mid-command.
		{"git status", false, ""},
		{"git push origin main", false, ""},
		{"rm -rf ~/trash", false, ""},
		{"notcurl --help", false, ""},
		{"echo curl is fun", false, ""},
	}
	for _, c := range cases {
		ok, src := m.Denied(c.cmd)
		if ok != c.ok || src != c.src {
			t.Errorf("Denied(%q) = (%v,%q), want (%v,%q)", c.cmd, ok, src, c.ok, c.src)
		}
	}
}

func TestDenyWinsOverAllow(t *testing.T) {
	// A command on BOTH sides must be reported denied: the engine checks the
	// deny side first, so the allowlist can never resurrect a blocked
	// command.
	m, err := New("", true, []string{"git status", "curl*"}, []string{"curl*"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.Allowed("git status"); !ok {
		t.Fatal("git status should stay allowed")
	}
	if ok, _ := m.Denied("curl -v http://x"); !ok {
		t.Fatal("curl must be denied even when allowlisted")
	}
	if ok, _ := m.Allowed("curl -v http://x"); !ok {
		t.Fatal("both sides may match; the caller must check Denied first")
	}
}

func TestDenylistIgnoresBlankAndBareStar(t *testing.T) {
	m, err := New("", true, nil, []string{"", "   ", "*", "wget*"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := m.Denied("anything at all"); ok {
		t.Fatal("bare '*' or blank entries must not block everything")
	}
	if ok, _ := m.Denied("wget http://x"); !ok {
		t.Fatal("valid prefix rule must still match")
	}
}

func TestRulesNormalizeWhitespace(t *testing.T) {
	// Rules are whitespace-normalized at insertion, so entries written with
	// interior runs of spaces/tabs still match the normalized command form.
	m, err := New("", true, []string{"git  status", "git   diff*"}, []string{"rm  -rf  /"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, src := m.Allowed("git status"); !ok || src != "exact" {
		t.Fatalf("multi-space exact rule must match: %v %q", ok, src)
	}
	if ok, src := m.Allowed("git diff HEAD"); !ok || src != "prefix" {
		t.Fatalf("multi-space prefix rule must match: %v %q", ok, src)
	}
	if ok, src := m.Denied("rm -rf /"); !ok || src != "exact" {
		t.Fatalf("multi-space deny rule must match: %v %q", ok, src)
	}
}

func TestDenyListExport(t *testing.T) {
	// Rules are stored in canonical lower-case form for case-insensitive
	// matching; the export shows them the same way, sorted.
	m, err := New("", true, []string{"git status"}, []string{"NC*", "rm -rf /", "curl*", "  Invoke-WebRequest*  "})
	if err != nil {
		t.Fatal(err)
	}
	exact, prefixes := m.DenyList()
	if len(exact) != 1 || exact[0] != "rm -rf /" {
		t.Fatalf("deny exact: %v", exact)
	}
	want := []string{"curl", "invoke-webrequest", "nc"}
	if len(prefixes) != len(want) {
		t.Fatalf("deny prefixes: %v", prefixes)
	}
	for i := range want {
		if prefixes[i] != want[i] {
			t.Fatalf("deny prefixes: %v, want %v", prefixes, want)
		}
	}
}
