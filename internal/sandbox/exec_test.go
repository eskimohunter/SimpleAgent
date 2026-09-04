package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTailWriter(t *testing.T) {
	tw := newTailWriter(10)
	tw.Write([]byte("0123456789"))
	if tw.String() != "0123456789" {
		t.Fatalf("got %q", tw.String())
	}
	tw.Write([]byte("ABCDE"))
	got := tw.String()
	if !strings.HasSuffix(got, "ABCDE") || len(got) > 10 {
		t.Fatalf("tail after overflow: %q", got)
	}
	if !tw.Dropped() {
		t.Fatal("expected dropped flag")
	}
}

func TestTailWriterDropsToLine(t *testing.T) {
	tw := newTailWriter(16)
	tw.Write([]byte("aaaaaaa\nbbbbbbbb\ncccc"))
	got := tw.String()
	if strings.HasPrefix(got, "aaaaaaa") {
		t.Fatalf("should drop first line, got %q", got)
	}
	if !strings.HasSuffix(got, "cccc") {
		t.Fatalf("got %q", got)
	}
}

func execSh(t *testing.T, cmdline string, timeout time.Duration, cap_ int) ExecResult {
	t.Helper()
	cfg := ExecConfig{
		Shell:          "sh",
		ShellArgs:      []string{"-c"},
		Timeout:        timeout,
		MaxOutputBytes: cap_,
		StripSecrets:   true,
	}
	res, err := Run(context.Background(), cfg, cmdline, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExecBasic(t *testing.T) {
	res := execSh(t, "echo hello-from-test", 5*time.Second, 4096)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello-from-test" {
		t.Fatalf("res=%+v", res)
	}
}

func TestExecExitCode(t *testing.T) {
	res := execSh(t, "exit 7", 5*time.Second, 4096)
	if res.ExitCode != 7 {
		t.Fatalf("got exit %d", res.ExitCode)
	}
}

func TestExecStartError(t *testing.T) {
	res, err := Run(context.Background(), ExecConfig{
		Shell: "definitely-not-a-real-shell-xyz", ShellArgs: []string{"-c"},
		Timeout: time.Second, MaxOutputBytes: 4096,
	}, "echo x", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StartError == nil {
		t.Fatal("expected start error")
	}
}

func TestExecTimeout(t *testing.T) {
	start := time.Now()
	res := execSh(t, "sleep 30", 300*time.Millisecond, 4096)
	if !res.TimedOut {
		t.Fatalf("expected timeout, res=%+v", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("kill took too long: %v", time.Since(start))
	}
}

func TestExecCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := ExecConfig{Shell: "sh", ShellArgs: []string{"-c"}, Timeout: time.Minute, MaxOutputBytes: 4096}
	done := make(chan ExecResult, 1)
	var res ExecResult
	go func() {
		r, _ := Run(ctx, cfg, "sleep 30", nil, nil)
		done <- r
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not kill process")
	}
	if !res.Canceled {
		t.Fatalf("expected canceled, res=%+v", res)
	}
}

func TestExecStripsSecrets(t *testing.T) {
	t.Setenv("FOO_TEST_TOKEN", "super-secret-value")
	t.Setenv("BAR_NORMAL", "plain-value")
	cfg := ExecConfig{
		Shell: "sh", ShellArgs: []string{"-c"},
		Timeout: 5 * time.Second, MaxOutputBytes: 4096, StripSecrets: true,
	}
	res, err := Run(context.Background(), cfg, "printf 'token=%s normal=%s' \"$FOO_TEST_TOKEN\" \"$BAR_NORMAL\"", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Stdout
	if strings.Contains(out, "super-secret") {
		t.Fatalf("secret leaked into child env: %q", out)
	}
	if !strings.Contains(out, "plain-value") {
		t.Fatalf("non-sensitive var should pass through: %q", out)
	}
}

func TestExecTruncation(t *testing.T) {
	res := execSh(t, "seq 1 50000", 10*time.Second, 2048)
	if !res.Truncated {
		t.Fatal("expected truncation flag")
	}
	if len(res.Stdout) > 2048+10 {
		t.Fatalf("tail too big: %d", len(res.Stdout))
	}
	if strings.Contains(res.Stdout, "1\n2\n3\n") {
		t.Fatalf("beginning of output must be dropped; head: %q", res.Stdout[:min(len(res.Stdout), 60)])
	}
	if !strings.HasSuffix(strings.TrimSpace(res.Stdout), "50000") {
		t.Fatalf("tail must contain the end of output: %q", res.Stdout[len(res.Stdout)-40:])
	}
}
