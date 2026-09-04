package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var sensitiveEnvParts = []string{
	"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL",
	"APIKEY", "API_KEY", "AUTH", "KEY", "PASSWD",
}

type ExecConfig struct {
	Shell          string
	ShellArgs      []string
	Dir            string
	TempDir        string
	Timeout        time.Duration
	MaxOutputBytes int
	StripSecrets   bool
}

type ExecResult struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	TimedOut   bool
	Canceled   bool
	Truncated  bool
	StartError error
	Elapsed    time.Duration
}

type tailWriter struct {
	mu   sync.Mutex
	buf  []byte
	cap_ int
	drop int64
}

func newTailWriter(capacity int) *tailWriter {
	return &tailWriter{cap_: capacity}
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if n >= t.cap_ {
		t.drop += int64(len(t.buf)) + int64(n) - int64(t.cap_)
		t.buf = append([]byte(nil), p[n-t.cap_:]...)
		return n, nil
	}
	room := t.cap_ - len(t.buf)
	if n > room {
		keep := t.cap_ - n
		t.drop += int64(len(t.buf) - keep)
		old := t.buf[len(t.buf)-keep:]
		merged := make([]byte, 0, t.cap_)
		merged = append(merged, old...)
		merged = append(merged, p...)
		t.buf = merged
		return n, nil
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) == 0 {
		return ""
	}
	out := string(t.buf)
	if t.drop > 0 {
		if i := strings.IndexByte(out, '\n'); i >= 0 && i < 256 {
			out = out[i+1:]
		}
	}
	return out
}

func (t *tailWriter) Dropped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.drop > 0
}

func filteredEnv(strip bool, tempDir string, extra []string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if strip {
			eq := strings.IndexByte(kv, '=')
			name := ""
			if eq >= 0 {
				name = kv[:eq]
			}
			if envIsSensitive(name) {
				continue
			}
		}
		out = append(out, kv)
	}
	if tempDir != "" {
		out = append(out, "TMP="+tempDir, "TEMP="+tempDir)
	}
	out = append(out, extra...)
	return out
}

func envIsSensitive(name string) bool {
	up := strings.ToUpper(name)
	for _, part := range sensitiveEnvParts {
		if strings.Contains(up, part) {
			return true
		}
	}
	return false
}

func Run(ctx context.Context, cfg ExecConfig, cmdline string, teeOut, teeErr func([]byte)) (ExecResult, error) {
	var res ExecResult
	if cfg.Shell == "" {
		return res, errors.New("no shell configured")
	}
	args := make([]string, 0, len(cfg.ShellArgs)+1)
	args = append(args, cfg.ShellArgs...)
	args = append(args, cmdline)
	cmd := exec.Command(cfg.Shell, args...)
	cmd.Dir = cfg.Dir
	cmd.Env = filteredEnv(cfg.StripSecrets, cfg.TempDir, nil)
	configureProcess(cmd)

	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 256 * 1024
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	outTail := newTailWriter(cfg.MaxOutputBytes)
	errTail := newTailWriter(cfg.MaxOutputBytes)

	stdoutP, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	stderrP, err := cmd.StderrPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		res.StartError = err
		return res, nil
	}

	var wg sync.WaitGroup
	start := time.Now()
	var killedOnce sync.Once
	var killCause atomicCause

	wg.Add(2)
	go func() {
		defer wg.Done()
		teeStdout(outTail, stdoutP, teeOut)
	}()
	go func() {
		defer wg.Done()
		teeStdout(errTail, stderrP, teeErr)
	}()

	doKill := func(cause string) {
		killedOnce.Do(func() {
			killCause.set(cause)
			_ = killProcessTree(cmd)
			_ = cmd.Process.Kill()
		})
	}

	timeoutTimer := time.AfterFunc(cfg.Timeout, func() { doKill("timeout") })
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			doKill("cancel")
		case <-ctxDone:
		}
	}()

	werr := cmd.Wait()
	timeoutTimer.Stop()
	close(ctxDone)
	res.Elapsed = time.Since(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}

	res.Stdout = outTail.String()
	res.Stderr = errTail.String()
	res.Truncated = outTail.Dropped() || errTail.Dropped()
	switch killCause.get() {
	case "timeout":
		res.TimedOut = true
	case "cancel":
		res.Canceled = true
	}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else if !res.TimedOut && !res.Canceled {
			return res, werr
		}
	} else {
		res.ExitCode = 0
	}
	return res, nil
}

type atomicCause struct {
	mu sync.Mutex
	v  string
}

func (a *atomicCause) set(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
}

func (a *atomicCause) get() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func teeStdout(t *tailWriter, src io.Reader, tee func([]byte)) {
	r := bufio.NewReaderSize(src, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = t.Write(buf[:n])
			if tee != nil {
				tee(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

func trimResult(s string) string {
	return strings.TrimSpace(strings.TrimRight(s, "\r\n"))
}

func FormatResult(res ExecResult) string {
	var b strings.Builder
	if res.StartError != nil {
		fmt.Fprintf(&b, "failed to start: %v", res.StartError)
		return b.String()
	}
	status := fmt.Sprintf("exit code %d", res.ExitCode)
	if res.TimedOut {
		status = "timed out and was killed (incl. child processes)"
	}
	if res.Canceled {
		status = "canceled by user"
	}
	if res.Truncated {
		status += " [output truncated]"
	}
	if res.Stdout != "" {
		fmt.Fprintf(&b, "[stdout]\n%s\n", trimResult(res.Stdout))
	}
	if res.Stderr != "" {
		fmt.Fprintf(&b, "[stderr]\n%s\n", trimResult(res.Stderr))
	}
	fmt.Fprintf(&b, "[status] %s in %s", status, res.Elapsed.Round(time.Millisecond))
	return b.String()
}
