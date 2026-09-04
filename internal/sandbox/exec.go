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

// sensitiveEnvParts lists substrings that, when found (case-insensitively)
// in an environment variable name, mark it as a secret. filteredEnv strips
// every matching variable from the child process environment - the model's
// API key and the user's credentials must never leak to a shell command,
// even an approved one (defense in depth on top of the approval gate).
var sensitiveEnvParts = []string{
	"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL",
	"APIKEY", "API_KEY", "AUTH", "KEY", "PASSWD",
}

// ExecConfig describes how to run one command.
type ExecConfig struct {
	Shell          string        // shell binary, e.g. powershell.exe or sh
	ShellArgs      []string      // fixed shell flags (see config.Defaults)
	Dir            string        // working directory (the project root)
	TempDir        string        // becomes TEMP/TMP in the child environment
	Timeout        time.Duration // kill the whole process tree after this
	MaxOutputBytes int           // bytes of stdout/stderr kept for the model
	StripSecrets   bool          // filter secret-looking env vars
}

// ExecResult is the outcome of one run_command, with the captured output
// and status flags describing HOW it ended (ran / timed out / cancelled).
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

// tailWriter is a tiny concurrency-safe byte ring that keeps only the LAST
// capacity bytes written to it, counting how many bytes were dropped. It is
// what caps command output: the child can produce megabytes, but at most the
// tail survives to reach the model. Writers call Write from several
// goroutines, hence the mutex.
type tailWriter struct {
	mu   sync.Mutex
	buf  []byte
	cap_ int
	drop int64
}

// newTailWriter creates a ring holding at most capacity bytes. The field is
// named cap_ (not cap) because "cap" is a builtin function in Go.
func newTailWriter(capacity int) *tailWriter {
	return &tailWriter{cap_: capacity}
}

// Write appends p, discarding the oldest bytes if the buffer would exceed
// capacity, and accounting dropped bytes in drop.
func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if n >= t.cap_ {
		// The chunk alone is larger than the whole ring: keep only its
		// own tail, everything previously buffered is dropped.
		t.drop += int64(len(t.buf)) + int64(n) - int64(t.cap_)
		t.buf = append([]byte(nil), p[n-t.cap_:]...)
		return n, nil
	}
	room := t.cap_ - len(t.buf)
	if n > room {
		// Not enough room: drop the oldest (len(buf)-keep) bytes, slide the
		// remaining suffix up, then append p at the end.
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

// String returns the buffered tail as text. When bytes were dropped, it
// additionally trims the first partial line (up to 256 bytes in) so the
// output does not begin mid-line with a fragment of dropped text.
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

// Dropped reports whether any byte was discarded (i.e. output exceeded the
// cap); the caller surfaces this as "[output truncated]".
func (t *tailWriter) Dropped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.drop > 0
}

// filteredEnv builds the child environment: it copies the parent's
// environment, optionally omitting secret-looking variables, then overrides
// TEMP/TMP to the harness tmp dir (so shell scratch files go to .agent/tmp,
// never the project) and appends any extra key=value pairs.
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

// envIsSensitive reports whether the variable name matches any sensitive
// substring (case-insensitively).
func envIsSensitive(name string) bool {
	up := strings.ToUpper(name)
	for _, part := range sensitiveEnvParts {
		if strings.Contains(up, part) {
			return true
		}
	}
	return false
}

// Run executes cmdline in the configured shell and captures its output.
// Approval is the CALLER's responsibility (agent.Engine gates this before
// calling); Run itself is the safe execution layer: environment hygiene,
// output caps, a hard timeout, tree kill and cancellation handling.
//
// The mechanics: the shell child is started with stdout/stderr pipes, two
// goroutines continuously drain those pipes into the tail buffers while
// teeing raw bytes to the UI (draining is mandatory - a child that fills a
// pipe nobody reads blocks forever). A timer and the caller's context race
// to kill the process tree on timeout or Ctrl+C; sync.Once guarantees the
// first kill wins. After the child exits, the readers get a short grace
// period to flush their buffers before results are assembled.
func Run(ctx context.Context, cfg ExecConfig, cmdline string, teeOut, teeErr func([]byte)) (ExecResult, error) {
	var res ExecResult
	if cfg.Shell == "" {
		return res, errors.New("no shell configured")
	}
	// The full command line is appended as one argument so the shell
	// interprets it (powershell -Command <line> / sh -c <line>).
	args := make([]string, 0, len(cfg.ShellArgs)+1)
	args = append(args, cfg.ShellArgs...)
	args = append(args, cmdline)
	cmd := exec.Command(cfg.Shell, args...)
	cmd.Dir = cfg.Dir
	cmd.Env = filteredEnv(cfg.StripSecrets, cfg.TempDir, nil)
	configureProcess(cmd) // platform-specific: process group + window hiding

	// Sensible defaults when the caller passed zeros.
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
	var killedOnce sync.Once // whichever kill reason fires first wins
	var killCause atomicCause

	// Two reader goroutines: each drains one pipe into its tail buffer and
	// forwards every byte to the tee callback (the UI) live.
	wg.Add(2)
	go func() {
		defer wg.Done()
		teeStdout(outTail, stdoutP, teeOut)
	}()
	go func() {
		defer wg.Done()
		teeStdout(errTail, stderrP, teeErr)
	}()

	// doKill kills the whole process tree (see killProcessTree in the
	// platform files). sync.Once makes concurrent killers harmless.
	doKill := func(cause string) {
		killedOnce.Do(func() {
			killCause.set(cause)
			_ = killProcessTree(cmd)
			_ = cmd.Process.Kill()
		})
	}

	// Two kill triggers race: the configured timeout firing, and the
	// caller's context being cancelled. ctxDone stops the watcher once the
	// command has finished on its own.
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

	// The readers may still hold buffered bytes that arrived right before
	// exit; give them up to 3 seconds to drain before we snapshot the tails.
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
			// The child ran and exited non-zero: that is a RESULT (with an
			// exit code), not an infrastructure error, so no Go error.
			res.ExitCode = ee.ExitCode()
		} else if !res.TimedOut && !res.Canceled {
			return res, werr
		}
	} else {
		res.ExitCode = 0
	}
	return res, nil
}

// atomicCause stores a small string under a mutex. A plain variable would
// race (the kill goroutine writes it while cmd.Wait reads), and sync/atomic
// has no string type, so a mutex is the simple correct answer here.
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

// teeStdout drains src, writing every chunk into the tail buffer and
// forwarding it to the tee callback. It returns when the pipe closes or
// errors - one goroutine per pipe, started by Run.
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

// trimResult strips surrounding whitespace and trailing newlines so the
// summary handed to the model does not carry dangling blank lines.
func trimResult(s string) string {
	return strings.TrimSpace(strings.TrimRight(s, "\r\n"))
}

// FormatResult renders an ExecResult as the text the model sees: stdout,
// stderr, then a [status] line describing exit code / timeout / cancel and
// the elapsed time, plus a truncation marker when output was cut.
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
