package config

import (
	"os"
	"path/filepath"
	"strings"
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

func remoteDefaults(t *testing.T) Config {
	t.Helper()
	c := Defaults()
	c.Root = "/tmp/x"
	c.Mock = false
	c.Model.BaseURL = "https://api.openrouter.ai/api/v1"
	c.Model.Model = "m"
	return c
}

func TestAPIKeyEnvPastedSecretFails(t *testing.T) {
	c := remoteDefaults(t)
	c.Model.APIKeyEnv = "sk-or-v1-abc-123"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "NAME of an environment variable") {
		t.Fatalf("expected env-name error, got %v", err)
	}
}

func TestRemoteHTTPSMissingKeyFails(t *testing.T) {
	c := remoteDefaults(t)
	c.Model.APIKeyEnv = "AGENT_MISSING_KEY_UNIQUE"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "environment variable is empty") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func TestRemoteHTTPSKeyPresentOK(t *testing.T) {
	c := remoteDefaults(t)
	t.Setenv("AGENT_PRESENT_KEY_UNIQUE", "sk-xyz")
	c.Model.APIKeyEnv = "AGENT_PRESENT_KEY_UNIQUE"
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRemoteHTTPSExplicitNoKeyOK(t *testing.T) {
	c := remoteDefaults(t)
	c.Model.APIKeyEnv = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("explicit no-key opt-out should pass: %v", err)
	}
}

func TestLoopbackHTTPSNoKeyOK(t *testing.T) {
	c := remoteDefaults(t)
	c.Model.BaseURL = "https://localhost:8443/v1"
	if err := c.Validate(); err != nil {
		t.Fatalf("loopback https should not require a key: %v", err)
	}
	c.Model.BaseURL = "https://127.0.0.1:8443/v1"
	if err := c.Validate(); err != nil {
		t.Fatalf("loopback https should not require a key: %v", err)
	}
}

func TestPrivateLANHTTPNoKeyOK(t *testing.T) {
	c := remoteDefaults(t)
	c.Model.BaseURL = "http://192.168.1.50:8000/v1"
	c.Model.APIKeyEnv = "AGENT_LAN_KEY"
	if err := c.Validate(); err != nil {
		t.Fatalf("private-LAN http should not require a key: %v", err)
	}
	c.Model.APIKeyEnv = "AGENT_LAN_KEY"
	t.Setenv("AGENT_LAN_KEY", "sekrit")
	if err := c.Validate(); err != nil {
		t.Fatalf("private-LAN http with key should pass: %v", err)
	}
}

func TestMockSkipsKeyChecks(t *testing.T) {
	c := Defaults()
	c.Root = "/tmp/x"
	c.Mock = true
	c.Model.BaseURL = ""
	c.Model.Model = ""
	c.Model.APIKeyEnv = "sk-or-v1-pasted"
	if err := c.Validate(); err != nil {
		t.Fatalf("mock mode must not require model config: %v", err)
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

func TestSessionInstructionConfig(t *testing.T) {
	d := Defaults()
	if !d.Session.ProjectInstructions {
		t.Fatal("project_instructions must default to true")
	}
	if d.Session.SystemPromptFile != "" {
		t.Fatalf("system_prompt_file must default to empty, got %q", d.Session.SystemPromptFile)
	}
	dir := t.TempDir()
	cfg := `{
		"model": {"base_url": "http://h/v1", "model": "m"},
		"session": {"project_instructions": false, "system_prompt_file": "custom.md"}
	}`
	if err := os.WriteFile(filepath.Join(dir, "simpleagent.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Session.ProjectInstructions {
		t.Fatal("project_instructions=false must override the default")
	}
	if c.Session.SystemPromptFile != "custom.md" {
		t.Fatalf("system_prompt_file not applied: %q", c.Session.SystemPromptFile)
	}
	// File overlay must not disturb the untouched defaults either way.
	if c.Session.MaxMessages != 200 {
		t.Fatal("defaults must survive file overlay")
	}
}

func TestDenylistDefaultsEmptyAndFileLoaded(t *testing.T) {
	if d := Defaults().Approvals.Denylist; d != nil && len(d) != 0 {
		t.Fatalf("denylist must default to empty, got %v", d)
	}
	dir := t.TempDir()
	cfg := `{
		"model": {"base_url": "http://h/v1", "model": "m"},
		"approvals": {"denylist": ["curl*", "wget*"]}
	}`
	if err := os.WriteFile(filepath.Join(dir, "simpleagent.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Approvals.Denylist
	if len(got) != 2 || got[0] != "curl*" || got[1] != "wget*" {
		t.Fatalf("denylist not applied from file: %v", got)
	}
}
