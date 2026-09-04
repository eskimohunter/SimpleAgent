package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

type ModelConfig struct {
	BaseURL            string  `json:"base_url"`
	Model              string  `json:"model"`
	APIKeyEnv          string  `json:"api_key_env"`
	InsecureSkipVerify bool    `json:"insecure_skip_verify"`
	Temperature        float64 `json:"temperature"`
	TimeoutSec         int     `json:"timeout_sec"`
}

type ShellConfig struct {
	Command           string   `json:"command"`
	Args              []string `json:"args"`
	DefaultTimeoutSec int      `json:"default_timeout_sec"`
	MaxOutputBytes    int      `json:"max_output_bytes"`
}

type FilesConfig struct {
	MaxReadBytes  int `json:"max_read_bytes"`
	MaxWriteBytes int `json:"max_write_bytes"`
}

type ApprovalsConfig struct {
	Allowlist []string `json:"allowlist"`
	Persist   bool     `json:"persist"`
}

type SessionConfig struct {
	MaxMessages         int `json:"max_messages"`
	MaxToolCallsPerTurn int `json:"max_tool_calls_per_turn"`
}

type Config struct {
	Root       string          `json:"root"`
	Model      ModelConfig     `json:"model"`
	Shell      ShellConfig     `json:"shell"`
	Files      FilesConfig     `json:"files"`
	Approvals  ApprovalsConfig `json:"approvals"`
	Session    SessionConfig   `json:"session"`
	Mock       bool            `json:"-"`
	ConfigPath string          `json:"-"`
}

func Defaults() Config {
	shellCmd := "sh"
	shellArgs := []string{"-c"}
	if runtime.GOOS == "windows" {
		shellCmd = "powershell.exe"
		shellArgs = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}
	}
	return Config{
		Root: "",
		Model: ModelConfig{
			BaseURL:            "",
			Model:              "",
			APIKeyEnv:          "AGENT_API_KEY",
			InsecureSkipVerify: false,
			Temperature:        0.2,
			TimeoutSec:         300,
		},
		Shell: ShellConfig{
			Command:           shellCmd,
			Args:              shellArgs,
			DefaultTimeoutSec: 120,
			MaxOutputBytes:    262144,
		},
		Files: FilesConfig{
			MaxReadBytes:  1 << 20,
			MaxWriteBytes: 5 << 20,
		},
		Approvals: ApprovalsConfig{
			Allowlist: nil,
			Persist:   true,
		},
		Session: SessionConfig{
			MaxMessages:         200,
			MaxToolCallsPerTurn: 25,
		},
	}
}

func (c *Config) applyFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	c.ConfigPath = path
	return nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("AGENT_BASE_URL"); v != "" {
		c.Model.BaseURL = v
	}
	if v := os.Getenv("AGENT_MODEL"); v != "" {
		c.Model.Model = v
	}
	if v := os.Getenv("AGENT_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Model.Temperature = f
		}
	}
}

func (c *Config) Validate() error {
	if c.Root == "" {
		return errors.New("project root is empty")
	}
	if !c.Mock {
		if c.Model.BaseURL == "" {
			return errors.New("model.base_url is required (set it in simpleagent.json or AGENT_BASE_URL)")
		}
		if c.Model.Model == "" {
			return errors.New("model.model is required (set it in simpleagent.json or AGENT_MODEL)")
		}
	}
	if c.Model.Temperature < 0 || c.Model.Temperature > 2 {
		return errors.New("model.temperature must be within [0,2]")
	}
	if c.Model.TimeoutSec <= 0 {
		return errors.New("model.timeout_sec must be positive")
	}
	if c.Shell.Command == "" {
		return errors.New("shell.command is empty")
	}
	if c.Shell.DefaultTimeoutSec <= 0 {
		return errors.New("shell.default_timeout_sec must be positive")
	}
	if c.Shell.MaxOutputBytes < 4096 {
		return errors.New("shell.max_output_bytes too small")
	}
	if c.Files.MaxReadBytes < 4096 || c.Files.MaxWriteBytes < 1024 {
		return errors.New("file size caps too small")
	}
	if c.Session.MaxMessages < 4 || c.Session.MaxToolCallsPerTurn < 1 {
		return errors.New("session limits too small")
	}
	return nil
}

func findDefaultConfig(root string) string {
	if root == "" {
		return ""
	}
	p := filepath.Join(root, "simpleagent.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func Load(flagRoot, flagConfig string, mock bool) (*Config, error) {
	c := Defaults()
	c.Mock = mock

	cfgPath := flagConfig
	if cfgPath == "" {
		if flagRoot != "" {
			cfgPath = findDefaultConfig(flagRoot)
		} else if p := findDefaultConfig("."); p != "" {
			cfgPath = p
		}
	}
	if cfgPath != "" {
		if err := c.applyFile(cfgPath); err != nil {
			return nil, err
		}
	}
	c.applyEnv()

	if flagRoot != "" {
		c.Root = flagRoot
	}
	if c.Root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		c.Root = cwd
	}

	abs, err := filepath.Abs(c.Root)
	if err != nil {
		return nil, err
	}
	c.Root = abs
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (m ModelConfig) APIKey() string {
	if m.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(m.APIKeyEnv)
}

func (m ModelConfig) MaskedBaseURL() string {
	if m.BaseURL == "" {
		return ""
	}
	u := m.BaseURL
	schemeEnd := len(u)
	if i := indexOf(u, "://"); i >= 0 {
		schemeEnd = i + 3
	}
	if m.APIKeyEnv == "" || m.APIKey() == "" {
		return u
	}
	return u[:schemeEnd] + "***@" + u[schemeEnd:]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
