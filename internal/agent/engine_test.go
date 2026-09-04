package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"simpleagent/internal/approvals"
	"simpleagent/internal/audit"
	"simpleagent/internal/config"
	"simpleagent/internal/mock"
	"simpleagent/internal/model"
	"simpleagent/internal/sandbox"
)

type fakeUI struct {
	approvals []string
	denyAll   bool
	decisions []approvals.Decision
}

func (u *fakeUI) StreamText(s string) {}
func (u *fakeUI) Notice(kind, msg string) {
}
func (u *fakeUI) ApproveCommand(ctx context.Context, cmd string) (approvals.Decision, error) {
	u.approvals = append(u.approvals, cmd)
	if u.denyAll {
		u.decisions = append(u.decisions, approvals.Deny)
		return approvals.Deny, nil
	}
	u.decisions = append(u.decisions, approvals.AllowOnce)
	return approvals.AllowOnce, nil
}

type scriptBrain func(msgs []model.Message) model.Reply

func brainFor(msgIdx func(msgs []model.Message) model.Reply) mock.Brain {
	return msgIdx
}

func testEngine(t *testing.T, brain mock.Brain, deny bool) (*Engine, *fakeUI, *sandbox.Sandbox) {
	t.Helper()
	dir := t.TempDir()
	root, err := sandbox.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := mock.New(brain)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	cfg := config.Defaults()
	cfg.Root = dir
	cfg.Model.Model = "mock"
	cfg.Model.BaseURL = "http://" + srv.Addr() + "/v1"

	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	allow, err := approvals.New("", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.New(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	ui := &fakeUI{denyAll: deny}
	client := model.NewClient(cfg.Model.BaseURL, "", false, 30*time.Second)
	eng := mustNew(t, cfg, client, sbx, allow, auditLog, ui, "test-session")
	eng.MaxToolCallsPerTurn = 10
	return eng, ui, sbx
}

// mustNew builds an engine like New, failing the test on startup errors
// (e.g. an unreadable explicit system_prompt_file).
func mustNew(t *testing.T, cfg config.Config, client *model.Client, sbx *sandbox.Sandbox, allow *approvals.Manager, auditLog *audit.Audit, ui UI, sessionID string) *Engine {
	t.Helper()
	eng, err := New(cfg, client, sbx, allow, auditLog, ui, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func TestEndToEndTools(t *testing.T) {
	toolResults := 0
	brain := func(msgs []model.Message) model.Reply {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == "tool" {
			toolResults++
		}
		switch toolResults {
		case 0:
			return model.Reply{
				ToolCalls: []model.ToolCall{{ID: "t1", Type: "function", Function: model.FunctionCall{
					Name: "list_files", Arguments: `{"path": ""}`}}},
			}
		case 1:
			return model.Reply{
				ToolCalls: []model.ToolCall{{ID: "t2", Type: "function", Function: model.FunctionCall{
					Name: "write_file", Arguments: `{"path": "out/hello.txt", "content": "hello agent\n"}`}}},
			}
		case 2:
			return model.Reply{
				Content: "all files written.",
			}
		default:
			return model.Reply{Content: "done."}
		}
	}
	eng, ui, sbx := testEngine(t, brain, false)
	sum, err := eng.RunTurn(context.Background(), "create a file")
	if err != nil {
		t.Fatal(err)
	}
	if sum.StopReason != "answer" || !strings.Contains(sum.AssistantText, "written") {
		t.Fatalf("summary: %+v", sum)
	}
	data, err := readRootFile(t, sbx, "out/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if data != "hello agent\n" {
		t.Fatalf("file content: %q", data)
	}
	if sum.FileCalls != 2 || sum.CommandCalls != 0 {
		t.Fatalf("call counts: %+v", sum)
	}
	if len(ui.approvals) != 0 {
		t.Fatal("file tools must not trigger approvals")
	}
}

func TestCommandApprovalFlow(t *testing.T) {
	brain := func(msgs []model.Message) model.Reply {
		if last := msgs[len(msgs)-1]; last.Role == "tool" {
			return model.Reply{Content: "command result received."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "echo safe-command"}`}}},
		}
	}
	eng, ui, _ := testEngine(t, brain, false)
	sum, err := eng.RunTurn(context.Background(), "run a command")
	if err != nil {
		t.Fatal(err)
	}
	if len(ui.approvals) != 1 || ui.approvals[0] != "echo safe-command" {
		t.Fatalf("approval requested for wrong command: %v", ui.approvals)
	}
	if sum.DeniedCommands != 0 {
		t.Fatal("command should have been allowed")
	}
}

func TestCommandDeniedRecovers(t *testing.T) {
	var sawResult string
	brain := func(msgs []model.Message) model.Reply {
		last := msgs[len(msgs)-1]
		if last.Role == "tool" {
			sawResult = stringOf(last)
			return model.Reply{Content: "understood, command denied."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "dangerous --flag"}`}}},
		}
	}
	eng, ui, _ := testEngine(t, brain, true)
	sum, err := eng.RunTurn(context.Background(), "do something risky")
	if err != nil {
		t.Fatal(err)
	}
	if len(ui.approvals) != 1 {
		t.Fatalf("approvals: %v", ui.approvals)
	}
	if sum.DeniedCommands != 1 {
		t.Fatalf("denials: %+v", sum)
	}
	if !strings.Contains(sawResult, "DENIED") {
		t.Fatalf("tool result must report denial: %q", sawResult)
	}
}

func TestAutoAllowFromAllowlistSkipsPrompt(t *testing.T) {
	dir := t.TempDir()
	root, _ := sandbox.NewRoot(dir)
	srv := mock.New(func(msgs []model.Message) model.Reply {
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "git status"}`}}},
		}
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cfg := config.Defaults()
	cfg.Root = dir
	cfg.Model.Model = "mock"
	cfg.Model.BaseURL = "http://" + srv.Addr() + "/v1"
	allow, _ := approvals.New("", false, []string{"git status"}, nil)
	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	auditLog, _ := audit.New(filepath.Join(dir, "audit.jsonl"))
	defer auditLog.Close()
	ui := &fakeUI{}
	client := model.NewClient(cfg.Model.BaseURL, "", false, 30*time.Second)
	eng := mustNew(t, cfg, client, sbx, allow, auditLog, ui, "s")
	eng.MaxToolCallsPerTurn = 3
	sum, err := eng.RunTurn(context.Background(), "check git status")
	if err != nil {
		t.Fatal(err)
	}
	if sum.DeniedCommands != 0 || len(ui.approvals) != 0 {
		t.Fatalf("allowlisted command must auto-run without prompt: %+v %v", sum, ui.approvals)
	}
}

func TestToolIterationLimit(t *testing.T) {
	brain := func(msgs []model.Message) model.Reply {
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("t%d", len(msgs)), Type: "function", Function: model.FunctionCall{
				Name: "list_files", Arguments: `{"path": ""}`}}},
		}
	}
	eng, _, _ := testEngine(t, brain, false)
	eng.MaxToolCallsPerTurn = 3
	sum, err := eng.RunTurn(context.Background(), "loop forever")
	if err != nil {
		t.Fatal(err)
	}
	if sum.StopReason != "tool-call limit reached" {
		t.Fatalf("stop reason: %+v", sum)
	}
}

func TestRollbackOnModelFailure(t *testing.T) {
	srv := mock.New(func(msgs []model.Message) model.Reply {
		return model.Reply{}
	})
	_ = srv
	// engine with dead server
	dir := t.TempDir()
	root, _ := sandbox.NewRoot(dir)
	cfg := config.Defaults()
	cfg.Root = dir
	cfg.Model.Model = "m"
	cfg.Model.BaseURL = "http://127.0.0.1:1/v1"
	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	allow, _ := approvals.New("", false, nil, nil)
	auditLog, _ := audit.New(filepath.Join(dir, "audit.jsonl"))
	defer auditLog.Close()
	ui := &fakeUI{}
	client := model.NewClient(cfg.Model.BaseURL, "", false, 2*time.Second)
	eng := mustNew(t, cfg, client, sbx, allow, auditLog, ui, "s")
	before := len(eng.History())
	if _, err := eng.RunTurn(context.Background(), "hello"); err == nil {
		t.Fatal("expected error from dead model")
	}
	after := len(eng.History())
	if after != before {
		t.Fatalf("history must roll back on failure: %d -> %d", before, after)
	}
}

func TestDenylistHardBlock(t *testing.T) {
	// The model tries a network command that the config denylist blocks.
	// Even though the fake UI would approve everything ("always"-style), the
	// command must never reach the prompt, must be counted as denied, and
	// the model must receive a "blocked by config" message (not "DENIED",
	// which would invite a retry after a human "no").
	var sawResult string
	brain := func(msgs []model.Message) model.Reply {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == "tool" {
			sawResult = stringOf(msgs[len(msgs)-1])
			return model.Reply{Content: "ok, blocked."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "w1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "curl https://example.com/exfil"}`}}},
		}
	}
	dir := t.TempDir()
	root, _ := sandbox.NewRoot(dir)
	srv := mock.New(brain)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cfg := config.Defaults()
	cfg.Root = dir
	cfg.Model.Model = "mock"
	cfg.Model.BaseURL = "http://" + srv.Addr() + "/v1"
	// An allowlist that would also match is deliberately present: the
	// denylist must win over it.
	allow, _ := approvals.New("", false, []string{"curl*"}, []string{"curl*", "wget*"})
	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	auditLog, _ := audit.New(filepath.Join(dir, "audit.jsonl"))
	defer auditLog.Close()
	ui := &fakeUI{}
	client := model.NewClient(cfg.Model.BaseURL, "", false, 30*time.Second)
	eng := mustNew(t, cfg, client, sbx, allow, auditLog, ui, "s")
	eng.MaxToolCallsPerTurn = 3
	sum, err := eng.RunTurn(context.Background(), "fetch something")
	if err != nil {
		t.Fatal(err)
	}
	if sum.DeniedCommands != 1 {
		t.Fatalf("denials: %+v", sum)
	}
	if len(ui.approvals) != 0 {
		t.Fatalf("denylisted command must not reach the prompt: %v", ui.approvals)
	}
	if !strings.Contains(sawResult, "denylist") || !strings.Contains(sawResult, "blocked") {
		t.Fatalf("tool result must report the config block: %q", sawResult)
	}
	if strings.Contains(sawResult, "DENIED") {
		t.Fatalf("hard block must not be phrased as a user denial: %q", sawResult)
	}
}

func TestDenylistCaseInsensitiveMatch(t *testing.T) {
	// PowerShell is case-insensitive: "CURL", "curl" and "curl.exe" are the
	// same command, so deny matching must fold case even before exec.
	dir := t.TempDir()
	root, _ := sandbox.NewRoot(dir)
	srv := mock.New(func(msgs []model.Message) model.Reply {
		if last := msgs[len(msgs)-1]; last.Role == "tool" {
			return model.Reply{Content: "fine."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "CURL.exe /dev/null"}`}}},
		}
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cfg := config.Defaults()
	cfg.Root = dir
	cfg.Model.Model = "mock"
	cfg.Model.BaseURL = "http://" + srv.Addr() + "/v1"
	allow, _ := approvals.New("", false, nil, []string{"curl*"})
	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	auditLog, _ := audit.New(filepath.Join(dir, "audit.jsonl"))
	defer auditLog.Close()
	ui := &fakeUI{}
	client := model.NewClient(cfg.Model.BaseURL, "", false, 30*time.Second)
	eng := mustNew(t, cfg, client, sbx, allow, auditLog, ui, "s")
	eng.MaxToolCallsPerTurn = 3
	sum, err := eng.RunTurn(context.Background(), "run uppercase curl")
	if err != nil {
		t.Fatal(err)
	}
	if sum.DeniedCommands != 1 || len(ui.approvals) != 0 {
		t.Fatalf("uppercase variant must be hard-blocked too: %+v %v", sum, ui.approvals)
	}
}

// buildEngine creates the full engine wiring (mock server, approvals, audit,
// fake UI) against a fresh temp project root, runs cfgFn to customize the
// config and/or write files into cfg.Root, and returns the engine or the
// New error. Success tests must check the error themselves. allowRules, when
// given, are installed as the approvals allowlist (deny rules are configured
// manually by the few tests that need them).
func buildEngine(t *testing.T, brain mock.Brain, cfgFn func(*config.Config), allowRules ...string) (*Engine, *fakeUI, error) {
	t.Helper()
	dir := t.TempDir()
	root, err := sandbox.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := mock.New(brain)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	cfg := config.Defaults()
	cfg.Root = dir
	cfg.Model.Model = "mock"
	cfg.Model.BaseURL = "http://" + srv.Addr() + "/v1"
	if cfgFn != nil {
		cfgFn(&cfg)
	}
	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	allow, err := approvals.New("", false, allowRules, nil)
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.New(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	ui := &fakeUI{}
	client := model.NewClient(cfg.Model.BaseURL, "", false, 30*time.Second)
	eng, err := New(cfg, client, sbx, allow, auditLog, ui, "s")
	return eng, ui, err
}

// captureSystem runs one turn with a brain that records the system message
// (messages[0]) on its first call and then answers, ending the turn.
func captureSystem(t *testing.T, eng *Engine) string {
	t.Helper()
	var got string
	brain := func(msgs []model.Message) model.Reply {
		if got == "" && len(msgs) > 0 {
			got = stringOf(msgs[0])
		}
		return model.Reply{Content: "ok."}
	}
	// Replace the engine's client with one pointed at a fresh mock server
	// wired to brain.
	srv := mock.New(brain)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	eng.Client = model.NewClient("http://"+srv.Addr()+"/v1", "", false, 30*time.Second)
	if _, err := eng.RunTurn(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("brain never saw a system message")
	}
	return got
}

func TestSystemPromptFromAGENTS(t *testing.T) {
	const marker = "PROJECT-RULES-AGENTS-MARKER"
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		if err := os.WriteFile(filepath.Join(cfg.Root, "AGENTS.md"), []byte("# rules\n"+marker+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := captureSystem(t, eng)
	if !strings.Contains(sys, "You are SimpleAgent") {
		t.Fatalf("default rules missing from system prompt")
	}
	if !strings.Contains(sys, marker) {
		t.Fatalf("AGENTS.md content missing from system prompt")
	}
	if strings.Index(sys, "You are SimpleAgent") > strings.Index(sys, marker) {
		t.Fatal("built-in rules must precede project instructions")
	}
}

func TestSystemPromptPrefersAGENTSOverCLAUDE(t *testing.T) {
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		root := cfg.Root
		if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("AGENTS-MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("CLAUDE-MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := captureSystem(t, eng)
	if !strings.Contains(sys, "AGENTS-MARKER") {
		t.Fatal("AGENTS.md must be preferred when both files exist")
	}
	if strings.Contains(sys, "CLAUDE-MARKER") {
		t.Fatal("CLAUDE.md must not be loaded when AGENTS.md exists")
	}
}

func TestSystemPromptFallsBackToCLAUDE(t *testing.T) {
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		if err := os.WriteFile(filepath.Join(cfg.Root, "CLAUDE.md"), []byte("CLAUDE-ONLY-MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := captureSystem(t, eng)
	if !strings.Contains(sys, "CLAUDE-ONLY-MARKER") {
		t.Fatal("CLAUDE.md fallback must be loaded when AGENTS.md is absent")
	}
}

func TestSystemPromptDiscoveryOptOut(t *testing.T) {
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		cfg.Session.ProjectInstructions = false
		if err := os.WriteFile(filepath.Join(cfg.Root, "AGENTS.md"), []byte("SHOULD-NOT-APPEAR\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if sys := captureSystem(t, eng); strings.Contains(sys, "SHOULD-NOT-APPEAR") {
		t.Fatal("project_instructions=false must disable auto-discovery")
	}
}

func TestSystemPromptExplicitFile(t *testing.T) {
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		// An AGENTS.md also exists: the explicit file must win over it.
		root := cfg.Root
		if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "notes", "prompt.md"), []byte("EXPLICIT-MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("AGENTS-MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.Session.SystemPromptFile = "notes/prompt.md"
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := captureSystem(t, eng)
	if !strings.Contains(sys, "EXPLICIT-MARKER") {
		t.Fatalf("explicit system_prompt_file (relative to root) must be loaded: %q", sys)
	}
	if strings.Contains(sys, "AGENTS-MARKER") {
		t.Fatal("explicit system_prompt_file must disable auto-discovery")
	}
}

func TestSystemPromptExplicitFileErrors(t *testing.T) {
	// Missing file: startup must fail, not silently fall back.
	if _, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		cfg.Session.SystemPromptFile = "does-not-exist.md"
	}); err == nil {
		t.Fatal("missing explicit system_prompt_file must error")
	}
	// Over the 64 KiB cap: startup must fail too.
	if _, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		cfg.Session.SystemPromptFile = "huge.md"
		if err := os.WriteFile(filepath.Join(cfg.Root, "huge.md"), []byte(strings.Repeat("x", 64*1024+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Fatal("oversized explicit system_prompt_file must error")
	}
}

func TestSystemPromptDiscoverySkipsSymlink(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "rules.md"), []byte("OUTSIDE-MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		if err := os.Symlink(filepath.Join(outside, "rules.md"), filepath.Join(cfg.Root, "AGENTS.md")); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if sys := captureSystem(t, eng); strings.Contains(sys, "OUTSIDE-MARKER") {
		t.Fatal("symlinked AGENTS.md must be skipped during discovery")
	}
}

func TestSystemPromptDiscoverySkipsOversized(t *testing.T) {
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		if err := os.WriteFile(filepath.Join(cfg.Root, "AGENTS.md"), []byte(strings.Repeat("x", 64*1024+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := captureSystem(t, eng)
	if strings.Contains(sys, strings.Repeat("x", 100)) {
		t.Fatal("oversized AGENTS.md must be skipped during discovery")
	}
}

func TestSystemPromptEmptyFileInjectsNothing(t *testing.T) {
	// A whitespace-only discovered AGENTS.md must not produce a bare empty
	// section...
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		if err := os.WriteFile(filepath.Join(cfg.Root, "AGENTS.md"), []byte("   \n  \n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if sys := captureSystem(t, eng); strings.Contains(sys, "Project instructions") {
		t.Fatalf("empty discovered file must inject no section: %q", sys)
	}
	// ...and an explicit empty system_prompt_file must neither error nor
	// append anything.
	eng2, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		if err := os.WriteFile(filepath.Join(cfg.Root, "empty.md"), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.Session.SystemPromptFile = "empty.md"
	})
	if err != nil {
		t.Fatal(err)
	}
	if sys := captureSystem(t, eng2); strings.Contains(sys, "Project instructions") {
		t.Fatalf("explicit empty file must inject no section: %q", sys)
	}
}

func TestSystemPromptUnreadableAGENTSFallsBackToCLAUDE(t *testing.T) {
	// A discovered AGENTS.md that exists but cannot be read (permissions)
	// must be skipped with a warning, not silently, and CLAUDE.md must then
	// be consulted - the fallback must not be masked by a broken file.
	eng, _, err := buildEngine(t, nil, func(cfg *config.Config) {
		root := cfg.Root
		agents := filepath.Join(root, "AGENTS.md")
		if err := os.WriteFile(agents, []byte("UNREADABLE-MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(agents, 0o000); err != nil {
			t.Fatal(err)
		}
		// Some filesystems (and Windows) do not enforce read permissions for
		// the owner; the fallback behavior is only observable when reading
		// really fails.
		if data, err := os.ReadFile(agents); err == nil {
			t.Skipf("permissions not enforced on this filesystem (still readable: %q)", data)
		}
		t.Cleanup(func() { _ = os.Chmod(agents, 0o644) })
		if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("CLAUDE-FALLBACK-MARKER\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	sys := captureSystem(t, eng)
	if !strings.Contains(sys, "CLAUDE-FALLBACK-MARKER") {
		t.Fatal("discovery must fall through to CLAUDE.md when AGENTS.md is unreadable")
	}
	if strings.Contains(sys, "UNREADABLE-MARKER") {
		t.Fatal("unreadable AGENTS.md content must not be injected")
	}
}

func TestPlanModeBlocksWrite(t *testing.T) {
	var saw string
	toolSeen := false
	brain := func(msgs []model.Message) model.Reply {
		for _, m := range msgs {
			if m.Role == "tool" {
				toolSeen = true
				saw = stringOf(m)
			}
		}
		if toolSeen {
			return model.Reply{Content: "ok, noted."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "w1", Type: "function", Function: model.FunctionCall{
				Name: "write_file", Arguments: `{"path": "victim.txt", "content": "x"}`}}},
		}
	}
	eng, ui, _ := mustBuild(t, brain, nil)
	eng.SetMode(ModePlan)
	sum, err := eng.RunTurn(context.Background(), "write a file")
	if err != nil {
		t.Fatal(err)
	}
	if sum.PlanBlocked != 1 || sum.FileCalls != 0 {
		t.Fatalf("plan write must be counted as blocked, not executed: %+v", sum)
	}
	if !strings.Contains(saw, "plan mode") {
		t.Fatalf("tool result must explain the plan block: %q", saw)
	}
	if _, err := readRootFile(t, eng.Sbx, "victim.txt"); err == nil {
		t.Fatal("file must not exist after a plan-mode write attempt")
	}
	if len(ui.approvals) != 0 {
		t.Fatal("file tools never prompt")
	}
}

func TestPlanModeBlocksCommandWithoutPrompt(t *testing.T) {
	var saw string
	toolSeen := false
	brain := func(msgs []model.Message) model.Reply {
		for _, m := range msgs {
			if m.Role == "tool" {
				toolSeen = true
				saw = stringOf(m)
			}
		}
		if toolSeen {
			return model.Reply{Content: "understood."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "echo side-effect"}`}}},
		}
	}
	eng, ui, _ := mustBuild(t, brain, nil)
	eng.SetMode(ModePlan)
	sum, err := eng.RunTurn(context.Background(), "run something")
	if err != nil {
		t.Fatal(err)
	}
	if sum.PlanBlocked != 1 || sum.DeniedCommands != 0 {
		t.Fatalf("counters: %+v", sum)
	}
	if sum.CommandCalls != 1 {
		t.Fatalf("the attempt itself must be counted: %+v", sum)
	}
	if len(ui.approvals) != 0 {
		t.Fatalf("plan mode must not prompt: %v", ui.approvals)
	}
	if !strings.Contains(saw, "plan mode") {
		t.Fatalf("tool result must explain the plan block: %q", saw)
	}
}

func TestPlanModeRunsAllowlistedCommand(t *testing.T) {
	var saw string
	toolSeen := false
	brain := func(msgs []model.Message) model.Reply {
		for _, m := range msgs {
			if m.Role == "tool" {
				toolSeen = true
				saw = stringOf(m)
			}
		}
		if toolSeen {
			return model.Reply{Content: "done."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "echo plan-ok"}`}}},
		}
	}
	eng, ui, _ := mustBuild(t, brain, nil, "echo*")
	eng.SetMode(ModePlan)
	sum, err := eng.RunTurn(context.Background(), "run the allowed command")
	if err != nil {
		t.Fatal(err)
	}
	if sum.PlanBlocked != 0 || sum.CommandCalls != 1 {
		t.Fatalf("allowlisted command must run in plan mode: %+v", sum)
	}
	if len(ui.approvals) != 0 {
		t.Fatalf("allowlisted command must not prompt: %v", ui.approvals)
	}
	if !strings.Contains(saw, "exit code 0") {
		t.Fatalf("command result missing: %q", saw)
	}
}

func TestPlanModeDenylistStillWins(t *testing.T) {
	// The denylist is checked before plan mode: a denied command reports the
	// deny rule, not the plan policy.
	var saw string
	toolSeen := false
	brain := func(msgs []model.Message) model.Reply {
		for _, m := range msgs {
			if m.Role == "tool" {
				toolSeen = true
				saw = stringOf(m)
			}
		}
		if toolSeen {
			return model.Reply{Content: "fine."}
		}
		return model.Reply{
			ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{
				Name: "run_command", Arguments: `{"command": "echo denied"}`}}},
		}
	}
	dir := t.TempDir()
	root, _ := sandbox.NewRoot(dir)
	srv := mock.New(brain)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cfg := config.Defaults()
	cfg.Root = dir
	cfg.Model.Model = "mock"
	cfg.Model.BaseURL = "http://" + srv.Addr() + "/v1"
	sbx := sandbox.NewSandbox(root, sandbox.FilesConfig{MaxReadBytes: 1 << 20, MaxWriteBytes: 1 << 20})
	allow, _ := approvals.New("", false, nil, []string{"echo*"})
	auditLog, _ := audit.New(filepath.Join(dir, "audit.jsonl"))
	defer auditLog.Close()
	ui := &fakeUI{}
	client := model.NewClient(cfg.Model.BaseURL, "", false, 30*time.Second)
	eng := mustNew(t, cfg, client, sbx, allow, auditLog, ui, "s")
	eng.SetMode(ModePlan)
	sum, err := eng.RunTurn(context.Background(), "run denied")
	if err != nil {
		t.Fatal(err)
	}
	if sum.DeniedCommands != 1 || sum.PlanBlocked != 0 {
		t.Fatalf("denylist must beat plan mode: %+v", sum)
	}
	if !strings.Contains(saw, "denylist") {
		t.Fatalf("denied command must report the deny rule: %q", saw)
	}
}

func TestPlanModeNoteInRequests(t *testing.T) {
	// While plan mode is active each request carries an ephemeral system
	// note, so the model learns the mode even with a long history.
	sawNote := false
	brain := func(msgs []model.Message) model.Reply {
		for _, m := range msgs {
			if m.Role == "system" && strings.Contains(stringOf(m), "CURRENT MODE: plan") {
				sawNote = true
			}
		}
		return model.Reply{Content: "plain answer."}
	}
	eng, _, _ := mustBuild(t, brain, nil)
	eng.SetMode(ModePlan)
	if _, err := eng.RunTurn(context.Background(), "plan something"); err != nil {
		t.Fatal(err)
	}
	if !sawNote {
		t.Fatal("plan-mode request must carry the ephemeral mode note")
	}
	if sys := stringOf(eng.History()[0]); strings.Contains(sys, "CURRENT MODE") {
		t.Fatal("mode note must not be persisted into the history")
	}
}

func TestToolsForMode(t *testing.T) {
	all := AllTools()
	if len(all) != 5 {
		t.Fatalf("build advertises 5 tools, got %d", len(all))
	}
	plan := toolsForMode(true)
	if len(plan) != 3 {
		t.Fatalf("plan advertises 3 tools, got %d", len(plan))
	}
	names := map[string]bool{}
	for _, tl := range plan {
		names[tl.Function.Name] = true
	}
	if !names["list_files"] || !names["read_file"] || !names["search_files"] {
		t.Fatalf("plan tool set wrong: %v", names)
	}
	if names["write_file"] || names["run_command"] {
		t.Fatal("write_file and run_command must be hidden in plan mode")
	}
	if got := toolsForMode(false); len(got) != 5 {
		t.Fatal("build mode must advertise everything")
	}
}

// mustBuild builds an engine that must start successfully, failing the test
// otherwise; see buildEngine for parameters.
func mustBuild(t *testing.T, brain mock.Brain, cfgFn func(*config.Config), allowRules ...string) (*Engine, *fakeUI, error) {
	t.Helper()
	eng, ui, err := buildEngine(t, brain, cfgFn, allowRules...)
	if err != nil {
		t.Fatal(err)
	}
	return eng, ui, err
}

func readRootFile(t *testing.T, sbx *sandbox.Sandbox, path string) (string, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sbx.Root().Abs(), filepath.FromSlash(path)))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func stringOf(m model.Message) string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}
