// Package repl is the human-facing side of the harness: a read-eval-print
// loop that reads user messages, runs agent turns, prints the streamed
// output, and handles the /commands and the Ctrl+C interrupt logic. The
// actual console drawing lives in ui.go (and the per-OS console_*.go files).
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

// REPL runs the interactive loop. sessionFile is the open JSONL file of the
// current conversation (see openSession); it is attached to the engine as
// its SessionLog so every message is persisted as it happens.
type REPL struct {
	cfg         *config.Config
	ui          *TextUI
	engine      *agent.Engine
	stateDir    string
	sessionFile *os.File
	version     string
}

// New creates the REPL and immediately opens a session file for the first
// conversation.
func New(cfg *config.Config, ui *TextUI, engine *agent.Engine, stateDir, version string) *REPL {
	r := &REPL{cfg: cfg, ui: ui, engine: engine, stateDir: stateDir, version: version}
	r.openSession()
	return r
}

// openSession starts a new conversation log: it closes any previous session
// file, creates .agent/sessions/ if needed, and opens a fresh JSONL file
// named by timestamp. The engine's SessionID is the file name, so entries
// in the session log and the audit trail refer to the same conversation.
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

// Run is the main REPL loop. It prints the banner, installs the Ctrl+C
// handler, then alternates between reading a user line and running one
// engine turn until the user exits.
func (r *REPL) Run() error {
	r.banner()
	// The signal goroutine gives Ctrl+C two meanings:
	//   - while a turn is running (turnCancel set): cancel that turn;
	//   - at the idle prompt: quit the program.
	// The first press calls cancel(); the engine returns promptly with
	// context.Canceled and turnCancel is cleared, so a second press exits.
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
				os.Exit(0)
			}
		}
	}()

	for {
		var line string
		var err error
		if r.ui.Interactive() {
			// Real terminal: raw-mode input so Tab toggles plan/build mode
			// instantly, and typing "/" opens the command menu (arrows to
			// select, Tab to accept, Enter to run, typing to filter).
			line, err = r.ui.ReadUserInteractive(r.prompt, r.toggleMode, slashMenu)
		} else {
			// Piped input (CI smoke tests etc.): cooked lines only; use the
			// /mode command to switch.
			r.ui.write(r.prompt())
			line, err = r.ui.ReadUserLine()
		}
		if err != nil {
			// EOF, read errors and Ctrl+C at the prompt all end the REPL.
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
		// Each user message runs in its own cancellable context; turnCancel
		// is published to the signal goroutine above while it is live.
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

// prompt returns the painted prompt for the current mode: green "user> " in
// build mode, yellow "plan> " in plan mode.
func (r *REPL) prompt() string {
	if r.engine.Mode() == agent.ModePlan {
		return r.ui.paint("1;33", "plan> ")
	}
	return r.ui.paint("1;32", "user> ")
}

// toggleMode flips plan/build mode and tells the user what changed.
func (r *REPL) toggleMode() {
	if r.engine.Mode() == agent.ModePlan {
		r.engine.SetMode(agent.ModeBuild)
		r.ui.Info("build mode: full file and command access. (Tab toggles plan mode)")
	} else {
		r.engine.SetMode(agent.ModePlan)
		r.ui.Info("plan mode: read-only. File writes and non-allowlisted commands are blocked. (Tab toggles build mode)")
	}
}

// banner prints the startup summary: version, project root, the model
// endpoint (credentials masked, or "mock" in demo mode) and state dir.
func (r *REPL) banner() {
	ep := "mock (built-in demo server)"
	if !r.cfg.Mock {
		ep = r.cfg.Model.MaskedBaseURL() + " model=" + r.cfg.Model.Model
	}
	r.ui.Info(fmt.Sprintf("SimpleAgent %s", r.version))
	r.ui.Info(fmt.Sprintf("  project root : %s", r.cfg.Root))
	r.ui.Info(fmt.Sprintf("  model        : %s", ep))
	r.ui.Info(fmt.Sprintf("  state        : %s", r.stateDir))
	if r.ui.Interactive() {
		r.ui.Info("  mode         : build (Tab toggles plan mode)")
	} else {
		r.ui.Info("  mode         : build (/mode toggles plan mode)")
	}
	r.ui.Info("  shell commands need your approval. type /help for commands.")
}

// slashCommand is one REPL command: the executable line and a short usage
// description. This table is the single source of truth for the /help text
// and for the interactive command menu shown when the user types "/".
type slashCommand struct {
	line  string
	usage string
}

// slashCommands lists the built-in commands in menu/help order.
var slashCommands = []slashCommand{
	{"/help", "this help"},
	{"/new", "clear conversation history (starts a new session file)"},
	{"/approvals", "show approval rules (allowlist + denylist)"},
	{"/mode", "show the current mode (plan/build); /mode plan or /mode build switches"},
	{"/exit", "quit"},
}

// slashMenu filters the command table by what the user typed after "/"
// (case-insensitive prefix match). It drives the interactive menu; an empty
// result means "no command matches - behave like plain text". Commands that
// end or reset the session (/exit, /new) are marked confirm so a stray
// Enter on a partial prefix cannot fire them (see menuEntry in ui.go).
func slashMenu(typed string) []menuEntry {
	prefix := strings.ToLower(typed)
	var out []menuEntry
	for _, c := range slashCommands {
		if strings.HasPrefix(c.line, prefix) {
			confirm := c.line == "/exit" || c.line == "/new"
			out = append(out, menuEntry{line: c.line, confirm: confirm})
		}
	}
	return out
}

// handleCommand dispatches a slash command. It returns quit=true when the
// REPL should exit, and an error when the failure is fatal (vs. just
// reporting an unknown command, which prints a hint instead).
func (r *REPL) handleCommand(line string) (bool, error) {
	parts := strings.Fields(line)
	cmd := strings.ToLower(parts[0])
	switch cmd {
	case "/help":
		var b strings.Builder
		b.WriteString("commands:")
		for _, c := range slashCommands {
			fmt.Fprintf(&b, "\n  %-12s %s", c.line, c.usage)
		}
		r.ui.Info(b.String())
		return false, nil
	case "/mode":
		r.handleMode(parts)
		return false, nil
	case "/new":
		// Reset the engine's in-memory history and rotate to a fresh
		// session file, so each conversation has its own JSONL.
		r.engine.Reset()
		r.openSession()
		r.ui.Info("session reset.")
		return false, nil
	case "/approvals":
		// Split presentation: allowlist prefix rules come from the config,
		// exact commands from the config allowlist and the user's "always"
		// answers; the denylist (always config, lower-cased) is printed last.
		exact, prefixes := r.engine.AllowList()
		dExact, dPrefixes := r.engine.DenyList()
		if len(exact) == 0 && len(prefixes) == 0 && len(dExact) == 0 && len(dPrefixes) == 0 {
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
			r.ui.Info("exact commands (allowlist / persisted approvals):")
			for _, c := range exact {
				r.ui.Info("  " + c)
			}
		}
		if len(dPrefixes) > 0 || len(dExact) > 0 {
			r.ui.Info("blocked commands (config denylist):")
			for _, p := range dPrefixes {
				r.ui.Info("  " + p + "*")
			}
			for _, c := range dExact {
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

// handleMode implements /mode: no argument prints the current mode, an
// argument of "plan" or "build" switches to it (via the same path the Tab
// toggle uses, including the audit event).
func (r *REPL) handleMode(parts []string) {
	want := ""
	if len(parts) > 1 {
		want = strings.ToLower(parts[1])
	}
	switch want {
	case "", "?":
		r.ui.Info("current mode: " + r.engine.Mode().String())
	case "plan", "build":
		m := agent.ModeBuild
		if want == "plan" {
			m = agent.ModePlan
		}
		if r.engine.Mode() != m {
			r.engine.SetMode(m)
			r.ui.Info("switched to " + want + " mode.")
		} else {
			r.ui.Info("already in " + want + " mode.")
		}
	default:
		r.ui.Error(`usage: /mode [plan|build]  (Tab also toggles the mode)`)
	}
}
