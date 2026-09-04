package repl

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"simpleagent/internal/agent"
	"simpleagent/internal/config"
)

type REPL struct {
	cfg         *config.Config
	ui          *TextUI
	engine      *agent.Engine
	stateDir    string
	sessionFile *os.File
	version     string
}

func New(cfg *config.Config, ui *TextUI, engine *agent.Engine, stateDir, version string) *REPL {
	r := &REPL{cfg: cfg, ui: ui, engine: engine, stateDir: stateDir, version: version}
	r.openSession()
	return r
}

func (r *REPL) openSession() {
	if r.sessionFile != nil {
		_ = r.sessionFile.Close()
	}
	dir := filepath.Join(r.stateDir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	r.sessionFile = f
	r.engine.SessionLog = f
	r.engine.SessionID = filepath.Base(path)
}

func (r *REPL) Run() error {
	r.banner()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	var turnCancel context.CancelFunc
	go func() {
		for range sig {
			if turnCancel != nil {
				c := turnCancel
				turnCancel = nil
				c()
			} else {
				fmt.Fprintln(os.Stdout, "\nbye")
				os.Exit(0)
			}
		}
	}()

	for {
		r.ui.write(r.ui.paint("1;32", "user> "))
		line, err := r.ui.ReadUserLine()
		if err != nil {
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			quit, err := r.handleCommand(line)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			continue
		}
		turnCtx, cancel := context.WithCancel(context.Background())
		turnCancel = cancel
		_, err = r.engine.RunTurn(turnCtx, line)
		turnCancel = nil
		cancel()
		if err != nil {
			if err == context.Canceled {
				r.ui.Error("(interrupted)")
			} else {
				r.ui.Error("error: " + err.Error())
			}
		}
	}
}

func (r *REPL) banner() {
	ep := "mock (built-in demo server)"
	if !r.cfg.Mock {
		ep = r.cfg.Model.MaskedBaseURL() + " model=" + r.cfg.Model.Model
	}
	r.ui.Info(fmt.Sprintf("SimpleAgent %s", r.version))
	r.ui.Info(fmt.Sprintf("  project root : %s", r.cfg.Root))
	r.ui.Info(fmt.Sprintf("  model        : %s", ep))
	r.ui.Info(fmt.Sprintf("  state        : %s", r.stateDir))
	r.ui.Info("  shell commands need your approval. type /help for commands.")
}

func (r *REPL) handleCommand(line string) (bool, error) {
	parts := strings.Fields(line)
	cmd := strings.ToLower(parts[0])
	switch cmd {
	case "/help":
		r.ui.Info(`commands:
  /help       this help
  /new        clear conversation history (starts a new session file)
  /approvals  show the current command allowlist
  /exit       quit`)
		return false, nil
	case "/new":
		r.engine.Reset()
		r.openSession()
		r.ui.Info("session reset.")
		return false, nil
	case "/approvals":
		exact, prefixes := r.engine.AllowList()
		if len(exact) == 0 && len(prefixes) == 0 {
			r.ui.Info("no approvals configured.")
			return false, nil
		}
		if len(prefixes) > 0 {
			r.ui.Info("prefix rules (config allowlist):")
			for _, p := range prefixes {
				r.ui.Info("  " + p + "*")
			}
		}
		if len(exact) > 0 {
			r.ui.Info("exact commands (persisted approvals):")
			for _, c := range exact {
				r.ui.Info("  " + c)
			}
		}
		return false, nil
	case "/exit":
		return true, nil
	default:
		r.ui.Error("unknown command: " + cmd + "  (try /help)")
		return false, nil
	}
}
