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
	eng := New(cfg, client, sbx, allow, auditLog, ui, "test-session")
	eng.MaxToolCallsPerTurn = 10
	return eng, ui, sbx
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
	eng := New(cfg, client, sbx, allow, auditLog, ui, "s")
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
	eng := New(cfg, client, sbx, allow, auditLog, ui, "s")
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
	eng := New(cfg, client, sbx, allow, auditLog, ui, "s")
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
	eng := New(cfg, client, sbx, allow, auditLog, ui, "s")
	eng.MaxToolCallsPerTurn = 3
	sum, err := eng.RunTurn(context.Background(), "run uppercase curl")
	if err != nil {
		t.Fatal(err)
	}
	if sum.DeniedCommands != 1 || len(ui.approvals) != 0 {
		t.Fatalf("uppercase variant must be hard-blocked too: %+v %v", sum, ui.approvals)
	}
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
