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

type UI interface {
	StreamText(string)
	Notice(kind, msg string)
	ApproveCommand(ctx context.Context, cmd string) (approvals.Decision, error)
}

type TurnSummary struct {
	UserText       string
	AssistantText  string
	ToolCalls      int
	FileCalls      int
	CommandCalls   int
	DeniedCommands int
	StopReason     string
}

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

func (e *Engine) AllowList() (exact, prefixes []string) {
	if e.Allow == nil {
		return nil, nil
	}
	return e.Allow.List()
}

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

func (e *Engine) Reset() {
	e.messages = e.messages[:1]
}

func (e *Engine) History() []model.Message {
	out := make([]model.Message, len(e.messages))
	copy(out, e.messages)
	return out
}

func (e *Engine) trimmedSlice() []model.Message {
	msgs := e.messages
	if len(msgs) <= e.MaxMessages {
		return msgs
	}
	keep := e.MaxMessages - 2
	if keep < 2 {
		keep = 2
	}
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

func (e *Engine) audit(kind string, data map[string]any) {
	if e.Audit == nil {
		return
	}
	_ = e.Audit.Log(kind, data)
}

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
			sum.AssistantText = reply.Content
			sum.StopReason = "answer"
			return sum, nil
		}

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
	sum.StopReason = "tool-call limit reached"
	if e.UI != nil {
		e.UI.Notice("warn", fmt.Sprintf("stopped after %d tool iterations; use /new to reset context", e.MaxToolCallsPerTurn))
	}
	return sum, nil
}

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
	}, cmdline,
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
