// Package config loads, merges and validates the harness configuration.
//
// Configuration is assembled in layers, each overriding the previous one:
// built-in defaults -> simpleagent.json (if present) -> AGENT_* environment
// variables -> explicit --root/--config flag values. All validation happens
// up front in Load, so the rest of the program can trust the Config it gets.
//
// Security-relevant rule: the API key is never stored in the config file.
// api_key_env holds the *name* of an environment variable that contains the
// key (see ModelConfig.APIKey and checkAPIKey).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// ModelConfig describes how to reach the OpenAI-compatible model server.
//
// APIKeyEnv is the NAME of an environment variable that holds the key
// (e.g. "AGENT_API_KEY") - never the key itself. When it is empty no
// Authorization header is sent. InsecureSkipVerify disables TLS certificate
// verification, which some self-hosted LAN servers with self-signed
// certificates require; keep it false unless you know why you need it.
type ModelConfig struct {
	BaseURL            string  `json:"base_url"`
	Model              string  `json:"model"`
	APIKeyEnv          string  `json:"api_key_env"`
	InsecureSkipVerify bool    `json:"insecure_skip_verify"`
	Temperature        float64 `json:"temperature"`
	TimeoutSec         int     `json:"timeout_sec"`
}

// ShellConfig controls how run_command invokes the operating system shell:
// Command/Args name the shell binary and its fixed flags (per-OS defaults
// come from Defaults), DefaultTimeoutSec is the kill-after timeout and
// MaxOutputBytes the cap on how much stdout/stderr is kept for the model.
type ShellConfig struct {
	Command           string   `json:"command"`
	Args              []string `json:"args"`
	DefaultTimeoutSec int      `json:"default_timeout_sec"`
	MaxOutputBytes    int      `json:"max_output_bytes"`
}

// FilesConfig caps the size of file reads and writes performed through the
// sandboxed file tools, protecting the model's context window and the disk.
type FilesConfig struct {
	MaxReadBytes  int `json:"max_read_bytes"`
	MaxWriteBytes int `json:"max_write_bytes"`
}

// ApprovalsConfig governs automatic command approval. Allowlist entries are
// exact commands, or prefixes when they end in "*"; matching commands never
// prompt. Denylist entries use the same syntax (exact or "*" prefix) but are
// HARD BLOCKS: a matching command never runs, not even with an interactive
// "always" approval - the denylist is checked before the allowlist and the
// prompt. Deny matching is case-insensitive (the default Windows shell,
// PowerShell, is case-insensitive). The denylist is configuration-only and
// never persisted in .agent/approvals.json. Persist controls whether
// commands the user approves with "always" survive restarts.
type ApprovalsConfig struct {
	Allowlist []string `json:"allowlist"`
	Denylist  []string `json:"denylist"`
	Persist   bool     `json:"persist"`
}

// SessionConfig bounds one interactive session: MaxMessages is how much
// history is kept in the model context (oldest messages are trimmed away)
// and MaxToolCallsPerTurn stops an agent that never stops calling tools.
type SessionConfig struct {
	MaxMessages         int `json:"max_messages"`
	MaxToolCallsPerTurn int `json:"max_tool_calls_per_turn"`
}

// Config is the fully merged configuration handed to the rest of the
// program. Mock and ConfigPath use `json:"-"` so they are never read from or
// written to a config file: Mock comes from the --mock flag and ConfigPath
// records where the file (if any) was loaded from.
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

// Defaults returns a fresh Config with built-in values. The shell is the one
// OS-dependent choice: `sh -c` on Unix, and on Windows powershell.exe with
// flags that make it non-interactive (no prompts, no profile scripts, no
// execution-policy interference) so commands can run unattended.
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

// applyFile merges a simpleagent.json file into the current config.
// json.Unmarshal only touches the keys present in the file, so any field the
// file omits silently keeps the value it already had (e.g. the defaults).
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

// applyEnv overlays the AGENT_BASE_URL / AGENT_MODEL / AGENT_TEMPERATURE
// environment variables, so a model can be pointed at without editing any
// file. Invalid temperature values are silently ignored (the file value or
// default stands).
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

// Validate checks the merged configuration for the values the rest of the
// program depends on: a non-empty root, a reachable model when not in mock
// mode, a sane API-key setup, and positive, bounded limits. It is the last
// step of Load, so errors here abort startup with a clear message.
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
		if err := c.checkAPIKey(); err != nil {
			return err
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

// findDefaultConfig looks for a simpleagent.json sitting next to the given
// root directory, and returns "" if there is none.
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

// Load builds the effective configuration: defaults, then the config file
// (--config, else auto-discovered next to the root), then environment
// overrides, then the --root flag which always wins. The root is converted to
// an absolute path and the result validated before being returned.
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

// APIKey returns the actual key by reading the environment variable whose
// name is stored in APIKeyEnv ("" if no variable is configured). This is the
// only place the key value enters the program - never the config file.
func (m ModelConfig) APIKey() string {
	if m.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(m.APIKeyEnv)
}

// MaskedBaseURL renders the base URL for the startup banner with any
// embedded credentials redacted (http://user:pass@host -> http://***@host).
// Called only when a key is actually configured, so secrets never print.
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

// indexOf reports the byte offset of the first occurrence of sub in s, or
// -1 if sub is absent. (Kept as a tiny helper so MaskedBaseURL does not need
// the strings package's allocs for one search.)
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// envNameRe matches what a valid POSIX environment variable name looks like.
// Used to tell a real variable NAME (AGENT_API_KEY) apart from a leaked key
// value that someone accidentally pasted into api_key_env.
func envNameRe() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
}

// checkAPIKey enforces the key-handling rules. It rejects configs whose
// api_key_env is not a plausible variable name (users sometimes paste the key
// itself here), and refuses to talk to a remote https endpoint without a key
// present. The one exemption: https servers on the local machine (loopback)
// such as localhost proxies are allowed to be keyless.
func (c *Config) checkAPIKey() error {
	if c.Model.APIKeyEnv == "" {
		return nil
	}
	if !envNameRe().MatchString(c.Model.APIKeyEnv) {
		return fmt.Errorf("model.api_key_env %q must be the NAME of an environment variable holding the key (e.g. \"AGENT_API_KEY\"), not the key itself", c.Model.APIKeyEnv)
	}
	if c.Mock {
		return nil
	}
	u, err := url.Parse(c.Model.BaseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("model.base_url %q is not a valid URL", c.Model.BaseURL)
	}
	if strings.EqualFold(u.Scheme, "https") && !isLoopbackHost(u.Host) && c.Model.APIKey() == "" {
		return fmt.Errorf("model.api_key_env %q is set but the environment variable is empty; export the key before starting (set api_key_env to \"\" if this endpoint genuinely needs no key)", c.Model.APIKeyEnv)
	}
	return nil
}

// isLoopbackHost reports whether hostport (a host, or host:port pair)
// refers to the local machine: "localhost" or a loopback IP such as
// 127.0.0.1 or ::1.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
