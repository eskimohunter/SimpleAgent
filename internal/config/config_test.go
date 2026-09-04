package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsPerOS(t *testing.T) {
	c := Defaults()
	if c.Shell.Command == "" || len(c.Shell.Args) == 0 {
		t.Fatal("shell defaults missing")
	}
	if c.Files.MaxReadBytes != 1<<20 {
		t.Fatal("read cap default wrong")
	}
	if !c.Approvals.Persist {
		t.Fatal("persist should default to true")
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"model": {"base_url": "http://192.168.1.50:8000/v1", "model": "mymodel"},
		"approvals": {"allowlist": ["git status"]}
	}`
	if err := os.WriteFile(filepath.Join(dir, "simpleagent.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.BaseURL != "http://192.168.1.50:8000/v1" || c.Model.Model != "mymodel" {
		t.Fatalf("file config not applied: %+v", c.Model)
	}
	if c.Model.Temperature != 0.2 {
		t.Fatal("defaults must survive file overlay")
	}
	if c.Root != dir {
		t.Fatalf("root: %s", c.Root)
	}
	if len(c.Approvals.Allowlist) != 1 {
		t.Fatal("allowlist missing")
	}
}

func TestValidate(t *testing.T) {
	c := Defaults()
	c.Root = "/tmp/x"
	c.Mock = false
	c.Model.BaseURL = ""
	if err := c.Validate(); err == nil {
		t.Fatal("missing base url must fail")
	}
	c.Model.BaseURL = "http://h/v1"
	c.Model.Model = "m"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Mock = true
	c.Model.BaseURL = ""
	c.Model.Model = ""
	if err := c.Validate(); err != nil {
		t.Fatal("mock mode should not require model config")
	}
}

func TestAPIKeyEnv(t *testing.T) {
	m := ModelConfig{APIKeyEnv: "AGENT_TEST_KEY_ENV"}
	t.Setenv("AGENT_TEST_KEY_ENV", "sekrit")
	if m.APIKey() != "sekrit" {
		t.Fatal("env key lookup failed")
	}
}

func TestFlagOverridesFileRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"model": {"base_url": "http://h/v1", "model": "m"}, "root": "/nonexistent/fromfile"}`
	file := filepath.Join(dir, "simpleagent.json")
	if err := os.WriteFile(file, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, file, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != dir {
		t.Fatalf("flag root must win over file root: %s", c.Root)
	}
}
