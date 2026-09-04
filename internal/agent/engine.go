// Package agent runs the model interaction loop: it keeps the conversation
// history, calls the model client, dispatches the tool calls the model
// requests, and integrates the two enforcement layers - the sandboxed file
// tools and the approval gate in front of shell commands.
//
// The Engine is the orchestrator; the actual tool definitions live in
// tools.go. The UI is injected as an interface so the engine never prints
// directly and can be driven headless in tests.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"simpleagent/internal/approvals"
	"simpleagent/internal/audit"
	"simpleagent/internal/config"
	"simpleagent/internal/model"
	"simpleagent/internal/sandbox"
)

// UI is everything the engine needs from the outside world that is not a
// tool: streaming the model's words as they arrive, showing notices (tool
// calls, command output, warnings), and asking the human whether a command
// may run. repl.TextUI is the only production implementation.
type UI interface {
	StreamText(string)
	Notice(kind, msg string)
	ApproveCommand(ctx context.Context, cmd string) (approvals.Decision, error)
}

// TurnSummary aggregates what happened during one user turn, so the REPL and
// tests can report or assert on activity without parsing the history.
type TurnSummary struct {
	UserText       string
	AssistantText  string
	ToolCalls      int
	FileCalls      int
	CommandCalls   int
	DeniedCommands int
	StopReason     string
}

// Engine owns one agent session: the conversation state and the loop that
// drives the model. The exported fields are dependencies wired in by the
// caller; the unexported fields are the session state (system prompt,
// message history, temp dir for commands).
type Engine struct {
	Client              *model.Client
	Sbx                 *sandbox.Sandbox
	Allow               *approvals.Manager
	Audit               *audit.Audit
	UI                  UI
	SessionLog          io.Writer
	SessionID           string
	MaxMessages         int
	MaxToolCallsPerTurn int

	cfg      config.Config
	system   model.Message
	messages []model.Message
	tempDir  string
}

// New wires a fresh engine: it snapshots the config the session should obey
// and seeds the history with the system prompt built for this project root.
func New(cfg config.Config, client *model.Client, sbx *sandbox.Sandbox, allow *approvals.Manager, auditLog *audit.Audit, ui UI, sessionID string) *Engine {
	e := &Engine{
		Client:              client,
		Sbx:                 sbx,
		Allow:               allow,
		Audit:               auditLog,
		UI:                  ui,
		SessionID:           sessionID,
		MaxMessages:         cfg.Session.MaxMessages,
		MaxToolCallsPerTurn: cfg.Session.MaxToolCallsPerTurn,
		cfg:                 cfg,
		tempDir:             filepath.Join(sbx.Root().Abs(), ".agent", "tmp"),
	}
	e.system = model.TextMessage("system", buildSystemPrompt(cfg.Root))
	e.messages = []model.Message{e.system}
	return e
}

// AllowList returns the current command auto-approvals, split into exact
// commands and prefixes, for the /approvals REPL command.
func (e *Engine) AllowList() (exact, prefixes []string) {
	if e.Allow == nil {
		return nil, nil
	}
	return e.Allow.List()
}

// buildSystemPrompt composes the model's standing instructions: the sandbox
// rules (relative paths only, .agent off-limits, no network), shell
// approval behavior, and how to use the file tools. The model reads these
// rules as text - they are advice - while the sandbox and approval gate
// enforce the same boundaries in code.
func buildSystemPrompt(root string) string {
	osName := runtime.GOOS
	if osName == "windows" {
		osName = "Windows 11"
	}
	shellName := "PowerShell"
	if runtime.GOOS != "windows" {
		shellName = "sh"
	}
	return fmt.Sprintf(`You are SimpleAgent, a coding assistant running on %s inside a strictly confined sandbox.

Project root: %s
Shell: %s (commands run non-interactively from the project root; no stdin is available).

Rules:
- All paths you handle are RELATIVE to the project root, using / as the separator. Never use absolute paths.
- You can only read and write files inside the project root. The harness state directory .agent/ is protected and invisible; ignore it.
- You have no network access and no access to anything outside the project root.
- Shell commands (run_command) are shown to the user and may require approval; they can be denied. If a command is denied, do not retry it - propose an alternative or explain.
- For inspecting code use search_files and read_file (line-numbered; pass offset/limit to page large files). Keep reads targeted; your context is limited.
- Use write_file for edits; it overwrites the whole file, so read first when modifying.
- Keep replies concise and concrete. Mention exact file paths when referring to files.
Today's date: %s`, osName, root, shellName, time.Now().Format("2006-01-02 15:04")) + "\n"
}

// Reset starts a fresh conversation: the history is truncated back to just
// the system prompt (index 0).
func (e *Engine) Reset() {
	e.messages = e.messages[:1]
}

// History returns a copy of the current message list, so callers cannot
// mutate the engine's internal history by accident.
func (e *Engine) History() []model.Message {
	out := make([]model.Message, len(e.messages))
	copy(out, e.messages)
	return out
}

// trimmedSlice returns the history capped to at most MaxMessages entries,
// for the model request. The trimming rules keep the protocol valid:
//   - the system message (index 0) is always kept;
//   - we drop whole leading entries, and never cut between an assistant
//     message with tool calls and the "tool" results that answer them -
//     the model API rejects an orphaned tool result.
//
// Note that the in-memory history is left untouched; only the request copy
// is trimmed.
func (e *Engine) trimmedSlice() []model.Message {
	msgs := e.messages
	if len(msgs) <= e.MaxMessages {
		return msgs
	}
	// Keep MaxMessages-2 entries so we never discard the pair (assistant
	// tool-call message + its tool results) sitting at the cut boundary.
	keep := e.MaxMessages - 2
	if keep < 2 {
		keep = 2
	}
	// Slide the start forward past any tool messages that would otherwise
	// end up orphaned at the front of the trimmed window.
	start := len(msgs) - keep
	for start < len(msgs) && msgs[start].Role == "tool" {
		start++
	}
	if start >= len(msgs) {
		start = len(msgs) - 1
	}
	out := make([]model.Message, 0, 2+(len(msgs)-start))
	out = append(out, msgs[0])
	out = append(out, msgs[start:]...)
	return out
}

// appendLog records a message in the in-memory history AND, when a session
// file is attached (SessionLog), appends it as one JSONL line for later
// inspection or replay.
func (e *Engine) appendLog(msg model.Message) {
	e.messages = append(e.messages, msg)
	if e.SessionLog == nil {
		return
	}
	line, err := json.Marshal(map[string]any{"ts": nowISO(), "session": e.SessionID, "message": msg})
	if err != nil {
		return
	}
	_, _ = e.SessionLog.Write(append(line, '\n'))
}

// audit writes one event to the audit trail when an auditor is configured.
// Callers ignore the error on purpose: auditing must never break the turn.
func (e *Engine) audit(kind string, data map[string]any) {
	if e.Audit == nil {
		return
	}
	_ = e.Audit.Log(kind, data)
}

// RunTurn processes one user message to completion. It runs the agent loop:
// send the history (plus tools) to the model, execute every tool call the
// model asks for, feed the results back, and repeat - until the model
// answers with plain text, the context is cancelled (Ctrl+C), or the
// MaxToolCallsPerTurn cap is hit so a runaway agent cannot loop forever.
//
// On error the history is rolled back to what it was before the turn, so a
// failed turn does not poison the next one.
func (e *Engine) RunTurn(ctx context.Context, userText string) (sum TurnSummary, err error) {
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return sum, errors.New("empty message")
	}
	rollback := len(e.messages)
	defer func() {
		if err != nil && len(e.messages) > rollback {
			e.messages = e.messages[:rollback]
		}
	}()
	sum.UserText = userText
	e.appendLog(model.TextMessage("user", userText))
	e.audit("user_message", map[string]any{"chars": len(userText)})

	for i := 0; i < e.MaxToolCallsPerTurn; i++ {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		req := model.ChatRequest{
			Model:       e.cfg.Model.Model,
			Messages:    e.trimmedSlice(),
			Tools:       AllTools(),
			Temperature: e.cfg.Model.Temperature,
		}
		// Stream the model's words to the UI live as they arrive.
		reply, err := e.Client.Chat(ctx, req, func(text string) {
			if e.UI != nil {
				e.UI.StreamText(text)
			}
		})
		if err != nil {
			return sum, fmt.Errorf("model request failed: %w", err)
		}
		e.appendLog(model.AssistantMessage(reply.Content, reply.ToolCalls))
		e.audit("assistant_reply", map[string]any{
			"chars":      len(reply.Content),
			"tool_calls": len(reply.ToolCalls),
			"finish":     reply.FinishReason,
		})

		if len(reply.ToolCalls) == 0 {
			// The model answered in plain text: the turn is done.
			sum.AssistantText = reply.Content
			sum.StopReason = "answer"
			return sum, nil
		}

		// Execute each requested tool and append its result to the history
		// so the model sees it on the next loop iteration.
		for _, tc := range reply.ToolCalls {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			content := e.dispatchTool(ctx, &sum, tc)
			e.appendLog(model.ToolResultMessage(tc.ID, content))
			e.audit("tool_result", map[string]any{
				"tool":  tc.Function.Name,
				"id":    tc.ID,
				"chars": len(content),
			})
		}
	}
	// Loop exited without a text answer: the per-turn tool-call cap hit.
	sum.StopReason = "tool-call limit reached"
	if e.UI != nil {
		e.UI.Notice("warn", fmt.Sprintf("stopped after %d tool iterations; use /new to reset context", e.MaxToolCallsPerTurn))
	}
	return sum, nil
}

// dispatchTool executes one tool call: it updates the summary counters,
// reports the call to the UI and audit log (with truncated args), decodes
// the JSON arguments, and routes to the file-tool handler or the command
// tool. Every path returns a string the model sees; errors become
// "ERROR: ..." strings rather than Go errors, because a failed tool call is
// still information the model should get.
func (e *Engine) dispatchTool(ctx context.Context, sum *TurnSummary, tc model.ToolCall) string {
	sum.ToolCalls++
	if e.UI != nil {
		e.UI.Notice("tool", fmt.Sprintf("%s %s", tc.Function.Name, truncateArgs(tc.Function.Arguments)))
	}
	e.audit("tool_call", map[string]any{
		"tool":    tc.Function.Name,
		"call_id": tc.ID,
		"args":    truncateArgs(tc.Function.Arguments),
	})
	// Cap the argument blob at 8 MB: anything larger is a hallucinated or
	// malicious call and is rejected before touching the sandbox.
	args, err := decodeArgs(tc.Function.Name, tc.Function.Arguments, 8*1024*1024)
	if err != nil {
		return err.Error()
	}
	if IsFileTool(tc.Function.Name) {
		sum.FileCalls++
		return e.runFileTool(ctx, tc.Function.Name, args)
	}
	switch tc.Function.Name {
	case "run_command":
		sum.CommandCalls++
		return e.runCommandTool(ctx, sum, args)
	default:
		return fmt.Sprintf("ERROR: unknown tool %q", tc.Function.Name)
	}
}

// runFileTool executes one of the sandboxed file tools (list/read/write/
// search). Each case pulls its arguments out of the decoded map and forwards
// them to the corresponding sandbox method; every sandbox call starts with a
// path-containment check, so no path the model invents can escape the
// project root.
func (e *Engine) runFileTool(ctx context.Context, name string, args map[string]json.RawMessage) string {
	if err := ctx.Err(); err != nil {
		return "ERROR: operation canceled"
	}
	switch name {
	case "list_files":
		path, err := getStr(args, "path")
		if err != nil {
			return err.Error()
		}
		recursive, err := getBool(args, "recursive")
		if err != nil {
			return err.Error()
		}
		out, err := e.Sbx.List(path, recursive, 2000)
		if err != nil {
			return fmt.Sprintf("ERROR: %v", err)
		}
		return out
	case "read_file":
		path, err := getStr(args, "path")
		if err != nil {
			return err.Error()
		}
		offset, err := getInt(args, "offset")
		if err != nil {
			return err.Error()
		}
		limit, err := getInt(args, "limit")
		if err != nil {
			return err.Error()
		}
		out, err := e.Sbx.ReadFile(path, offset, limit)
		if err != nil {
			return fmt.Sprintf("ERROR: %v", err)
		}
		return out
	case "write_file":
		path, err := getStr(args, "path")
		if err != nil {
			return err.Error()
		}
		content, err := getStr(args, "content")
		if err != nil {
			return err.Error()
		}
		append_, err := getBool(args, "append")
		if err != nil {
			return err.Error()
		}
		out, err := e.Sbx.WriteFile(path, content, append_)
		if err != nil {
			return fmt.Sprintf("ERROR: %v", err)
		}
		return out
	case "search_files":
		pattern, err := getStr(args, "pattern")
		if err != nil {
			return err.Error()
		}
		include, err := getStr(args, "include")
		if err != nil {
			return err.Error()
		}
		out, err := e.Sbx.Search(pattern, include, 500)
		if err != nil {
			return fmt.Sprintf("ERROR: %v", err)
		}
		return out
	}
	return fmt.Sprintf("ERROR: unknown file tool %q", name)
}

// runCommandTool executes the approval-gated shell tool. Order matters:
// the command may only reach the sandbox executor after the human approved
// it (or the allowlist matched). The result string is assembled from the
// sandbox result so the model sees stdout/stderr plus an exit status.
func (e *Engine) runCommandTool(ctx context.Context, sum *TurnSummary, args map[string]json.RawMessage) string {
	if err := ctx.Err(); err != nil {
		return "ERROR: operation canceled"
	}
	cmdline, err := getStr(args, "command")
	if err != nil {
		return err.Error()
	}
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return "ERROR: empty command"
	}
	// A tool-supplied timeout overrides the config default, but is clamped
	// to 600 seconds so the model cannot disable the kill switch.
	timeoutSec, err := getInt(args, "timeout_sec")
	if err != nil {
		return err.Error()
	}
	timeout := time.Duration(e.cfg.Shell.DefaultTimeoutSec) * time.Second
	if timeoutSec > 0 {
		if timeoutSec > 600 {
			timeoutSec = 600
		}
		timeout = time.Duration(timeoutSec) * time.Second
	}

	// THE approval gate: allowlist match first, then the interactive prompt.
	approved, remember := e.approveCommand(ctx, cmdline)
	if !approved {
		sum.DeniedCommands++
		e.audit("approval_denied", map[string]any{"command": cmdline})
		if e.UI != nil {
			e.UI.Notice("deny", "denied command: "+cmdline)
		}
		return fmt.Sprintf("The user DENIED this command and it did not run: %q. Propose an alternative or explain why it is not needed.", cmdline)
	}
	if remember {
		e.audit("approval_granted", map[string]any{"command": cmdline, "scope": "always"})
	} else {
		e.audit("approval_granted", map[string]any{"command": cmdline, "scope": "once"})
	}

	if e.UI != nil {
		e.UI.Notice("run", cmdline)
	}
	res, runErr := sandbox.Run(ctx, sandbox.ExecConfig{
		Shell:          e.cfg.Shell.Command,
		ShellArgs:      e.cfg.Shell.Args,
		Dir:            e.Sbx.Root().Abs(),
		TempDir:        e.tempDir,
		Timeout:        timeout,
		MaxOutputBytes: e.cfg.Shell.MaxOutputBytes,
		StripSecrets:   true,
	},
		// The two callbacks stream the child's stdout/stderr to the UI live,
		// so the user watches the command run while it runs.
		cmdline,
		func(p []byte) {
			if e.UI != nil {
				e.UI.Notice("out", string(p))
			}
		},
		func(p []byte) {
			if e.UI != nil {
				e.UI.Notice("err", string(p))
			}
		},
	)
	e.audit("command_result", map[string]any{
		"command":    cmdline,
		"exit":       res.ExitCode,
		"timed_out":  res.TimedOut,
		"canceled":   res.Canceled,
		"truncated":  res.Truncated,
		"elapsed_ms": res.Elapsed.Milliseconds(),
	})
	if runErr != nil && !res.Canceled && !res.TimedOut {
		return "ERROR: " + runErr.Error()
	}
	if res.Canceled {
		return "[canceled] the command was interrupted by the user; it did not complete."
	}
	return sandbox.FormatResult(res)
}

// approveCommand decides whether cmdline may run. Order of checks:
//  1. configured/persisted allowlist -> run without asking ("once" scope);
//  2. no UI attached (headless) -> deny, since no human can approve;
//  3. interactive prompt: yes-once / always / no.
//
// remember reports whether the user chose "always" so the caller can persist
// the command (it is also recorded by Remember inside the switch).
func (e *Engine) approveCommand(ctx context.Context, cmdline string) (approved, remember bool) {
	if e.Allow != nil {
		if ok, _ := e.Allow.Allowed(cmdline); ok {
			return true, false
		}
	}
	if e.UI == nil {
		return false, false
	}
	dec, err := e.UI.ApproveCommand(ctx, cmdline)
	if err != nil {
		return false, false
	}
	switch dec {
	case approvals.AllowOnce:
		return true, false
	case approvals.AllowAlways:
		if e.Allow != nil {
			_ = e.Allow.Remember(cmdline)
		}
		return true, true
	default:
		return false, false
	}
}
