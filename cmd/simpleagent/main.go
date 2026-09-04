// Command simpleagent runs the SimpleAgent coding harness. All the wiring
// between internal packages lives in this one file, so it is the best map of
// the whole program (see docs/code-walkthrough.md for a guided reading order).
//
// Startup at a glance:
//
//	flags -> config (defaults -> file -> env) -> project-root sandbox and
//	.agent state directory -> audit log -> approvals store -> model client
//	(or the built-in mock server with --mock) -> engine + text UI + REPL loop.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"simpleagent/internal/agent"
	"simpleagent/internal/approvals"
	"simpleagent/internal/audit"
	"simpleagent/internal/config"
	"simpleagent/internal/mock"
	"simpleagent/internal/model"
	"simpleagent/internal/repl"
	"simpleagent/internal/sandbox"
)

// version is stamped at build time by the Makefile via
// `-ldflags "-X main.version=..."`; this literal is only the fallback for
// plain `go build` / `go run`.
var version = "0.1.0"

// stateDirName is the harness state directory. It lives inside the project
// root (so the state travels with the project) but is hidden from the model's
// file tools and reserved exclusively for the harness.
const stateDirName = ".agent"

// main is a thin wrapper around run(): printing the error and exiting with a
// non-zero status is all that belongs here. Keeping the real logic in run()
// (which can return an error and use defer for cleanup) is idiomatic Go and
// keeps the startup sequence easy to follow.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "simpleagent:", err)
		os.Exit(1)
	}
}

// run performs the whole startup sequence and then hands control to the
// REPL, which blocks until the user quits. It returns an error whenever the
// harness cannot be set up, so main can report it in one place.
func run() error {
	flagRoot := flag.String("root", "", "project root directory (default: current directory)")
	flagConfig := flag.String("config", "", "path to a simpleagent.json config file")
	flagMock := flag.Bool("mock", false, "use the built-in mock model server (offline demo; no config needed)")
	flagVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *flagVersion {
		fmt.Println("simpleagent", version)
		return nil
	}

	// Load and validate configuration. Precedence, lowest to highest:
	// built-in defaults -> simpleagent.json (auto-discovered in the root or
	// taken from --config) -> AGENT_* environment variables -> --root flag.
	cfg, err := config.Load(*flagRoot, *flagConfig, *flagMock)
	if err != nil {
		return err
	}

	// Resolve the project root to its real filesystem path (following any
	// symlink on the way) so the root itself cannot be an escape point.
	// Every file tool the model can call is confined to this directory.
	root, err := sandbox.NewRoot(cfg.Root)
	if err != nil {
		return err
	}
	// Prepare the harness state directory. "tmp" becomes the TEMP/TMP for
	// shell commands (scratch files never pollute the project), "sessions"
	// holds one JSONL file per conversation, and the audit log lives directly
	// in stateDir.
	stateDir := filepath.Join(root.Abs(), stateDirName)
	for _, d := range []string{stateDir, filepath.Join(stateDir, "sessions"), filepath.Join(stateDir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// The audit log is an append-only JSONL trail of every tool call and
	// approval decision. It is flushed after every event so a crash loses at
	// most the event in flight. defer runs Close() when run() returns.
	auditLog, err := audit.New(filepath.Join(stateDir, "audit.jsonl"))
	if err != nil {
		return err
	}
	defer auditLog.Close()
	_ = auditLog.Log("session_start", map[string]any{
		"version": version,
		"root":    root.Abs(),
		"mock":    cfg.Mock,
	})

	// Load the approval manager: commands that match the configured prefix
	// allowlist, or previously "always"-approved commands (persisted in
	// approvals.json), run without an approval prompt. Commands matching
	// the config denylist are hard-blocked before anything else.
	allow, err := approvals.New(
		filepath.Join(stateDir, "approvals.json"),
		cfg.Approvals.Persist,
		cfg.Approvals.Allowlist,
		cfg.Approvals.Denylist,
	)
	if err != nil {
		return err
	}

	// The model client speaks the OpenAI-compatible chat API over HTTP.
	// APIKey() reads the key from the environment variable *named* in the
	// config; the key itself never sits in the config file.
	client := model.NewClient(
		cfg.Model.BaseURL,
		cfg.Model.APIKey(),
		cfg.Model.InsecureSkipVerify,
		time.Duration(cfg.Model.TimeoutSec)*time.Second,
	)
	// --mock swaps the real model for the built-in scripted server on
	// 127.0.0.1 (a random free port), so the whole harness can be exercised
	// offline with no API key and no model config. Everything downstream just
	// talks HTTP to a different base URL and cannot tell the difference.
	if cfg.Mock {
		srv := mock.New(nil)
		if err := srv.Start(); err != nil {
			return err
		}
		defer srv.Close()
		cfg.Model.BaseURL = "http://" + srv.Addr() + "/v1"
		cfg.Model.Model = "mock-agent"
		client = model.NewClient(cfg.Model.BaseURL, "", false, 60*time.Second)
		_ = auditLog.Log("mock_server", map[string]any{"addr": srv.Addr()})
	}

	// The sandbox applies the file size caps and gates every file operation
	// through the path-containment check in internal/sandbox/paths.go.
	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{
		MaxReadBytes:  cfg.Files.MaxReadBytes,
		MaxWriteBytes: cfg.Files.MaxWriteBytes,
	})

	// The engine owns the conversation history, runs the agent loop and
	// dispatches tool calls. The REPL opens one session JSONL file per
	// conversation and attaches it to the engine as SessionLog; the ID is
	// derived from the wall clock so session files never collide.
	sessionID := time.Now().Format("20060102-150405")
	ui := repl.NewTextUI(os.Stdin, os.Stdout)
	engine := agent.New(*cfg, client, sbx, allow, auditLog, ui, sessionID)
	r := repl.New(cfg, ui, engine, stateDir, version)
	return r.Run()
}
