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

var version = "0.1.0"

const stateDirName = ".agent"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "simpleagent:", err)
		os.Exit(1)
	}
}

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

	cfg, err := config.Load(*flagRoot, *flagConfig, *flagMock)
	if err != nil {
		return err
	}

	root, err := sandbox.NewRoot(cfg.Root)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(root.Abs(), stateDirName)
	for _, d := range []string{stateDir, filepath.Join(stateDir, "sessions"), filepath.Join(stateDir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

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

	allow, err := approvals.New(
		filepath.Join(stateDir, "approvals.json"),
		cfg.Approvals.Persist,
		cfg.Approvals.Allowlist,
	)
	if err != nil {
		return err
	}

	client := model.NewClient(
		cfg.Model.BaseURL,
		cfg.Model.APIKey(),
		cfg.Model.InsecureSkipVerify,
		time.Duration(cfg.Model.TimeoutSec)*time.Second,
	)
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

	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{
		MaxReadBytes:  cfg.Files.MaxReadBytes,
		MaxWriteBytes: cfg.Files.MaxWriteBytes,
	})

	sessionID := time.Now().Format("20060102-150405")
	ui := repl.NewTextUI(os.Stdin, os.Stdout)
	engine := agent.New(*cfg, client, sbx, allow, auditLog, ui, sessionID)
	r := repl.New(cfg, ui, engine, stateDir, version)
	return r.Run()
}
