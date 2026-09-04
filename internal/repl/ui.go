package repl

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"simpleagent/internal/approvals"
)

// TextUI renders the console: prompts, streaming model output, tool notices,
// command output and approval prompts. It is the production implementation
// of the agent.UI interface (see internal/agent/engine.go).
//
// Concurrency note: output arrives from several goroutines (the engine's
// turn, the command output tee, and the main loop's prompts), so every write
// is serialized through mu. The streaming flag additionally tracks whether
// the model's text is mid-line, so prompts and notices always start on a
// fresh line instead of being glued onto streamed words.
type TextUI struct {
	mu        sync.Mutex
	in        *bufio.Reader
	out       io.Writer
	color     bool
	streaming bool
}

// NewTextUI wraps an input reader and output writer. When writing to a real
// terminal it enables Windows console support (UTF-8 + ANSI colors) and
// turns on ANSI color unless the NO_COLOR environment variable is set.
func NewTextUI(in io.Reader, out io.Writer) *TextUI {
	if out == os.Stdout {
		configureConsole()
	}
	color := false
	if f, ok := out.(*os.File); ok && isTTY(f) && os.Getenv("NO_COLOR") == "" {
		color = true
	}
	return &TextUI{in: bufio.NewReader(in), out: out, color: color}
}

// isTTY reports whether f is a character device (a terminal). Piped output
// (e.g. `simpleagent | tee log`) is not a TTY, so colors stay off and the
// log is not polluted with ANSI escapes.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// paint wraps s in an ANSI SGR escape sequence (code like "1;32" =
// bold green) when colors are enabled, e.g. "\x1b[1;32mtext\x1b[0m".
func (u *TextUI) paint(code, s string) string {
	if !u.color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// write is the low-level, mutex-guarded print used by everything else.
func (u *TextUI) write(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprint(u.out, s)
}

// Notice implements agent.UI.Notice. Two kinds of payloads exist:
//   - kind "out"/"err": raw byte chunks of a command's stdout/stderr,
//     forwarded verbatim (styled) so the stream renders live;
//   - anything else ("tool", "run", "deny", "warn"): a labeled, formatted
//     one-line event that starts on its own line.
func (u *TextUI) Notice(kind, msg string) {
	if kind == "out" || kind == "err" {
		var prefix string
		switch kind {
		case "err":
			prefix = u.paint("31", msg) // red: stderr
		default:
			prefix = u.paint("2", msg) // dim: stdout
		}
		u.write(prefix)
		return
	}
	var text string
	switch kind {
	case "tool":
		text = u.paint("36", "» "+msg) + "\n" // cyan: model tool call
	case "run":
		text = u.paint("1;36", "$ "+msg) + "\n" // bold cyan: approved command
	case "deny":
		text = u.paint("33", "denied: "+msg) + "\n" // yellow: denied command
	case "warn":
		text = u.paint("33", msg) + "\n"
	default:
		text = msg + "\n"
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		// The model's streamed text is mid-line: drop to a fresh line first.
		fmt.Fprintln(u.out)
		u.streaming = false
	}
	fmt.Fprint(u.out, text)
}

// StreamText implements agent.UI.StreamText: prints a fragment of the
// model's answer as it arrives and remembers that a line is being drawn.
func (u *TextUI) StreamText(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.streaming = true
	fmt.Fprint(u.out, s)
}

// Info prints a dim informational line (banner, help text, notices).
func (u *TextUI) Info(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		fmt.Fprintln(u.out)
		u.streaming = false
	}
	fmt.Fprint(u.out, u.paint("2", s), "\n")
}

// Error prints a red error line, first ending any in-progress stream.
func (u *TextUI) Error(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		fmt.Fprintln(u.out)
		u.streaming = false
	}
	fmt.Fprint(u.out, u.paint("31", s), "\n")
}

// FinishStream ends any half-drawn streamed line with a newline. Called
// before the UI draws a prompt, so user input never shares a line with
// model output.
func (u *TextUI) FinishStream() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		fmt.Fprintln(u.out)
		u.streaming = false
	}
}

// ApproveCommand implements agent.UI.ApproveCommand: it draws the command
// and prompts y/a/n. The select on ctx.Done() polls cancellation (Ctrl+C
// during a turn) so a cancelled turn does not leave the prompt spinning.
func (u *TextUI) ApproveCommand(ctx context.Context, cmd string) (approvals.Decision, error) {
	u.FinishStream()
	u.write(u.paint("1;33", "approve command?") + " " + u.paint("2", "[y]es once / [a]lways / [n]o") + "\n")
	u.write(u.paint("2", "    ") + cmd + "\n")
	for {
		select {
		case <-ctx.Done():
			return approvals.Deny, ctx.Err()
		default:
		}
		u.write(u.paint("33", "y/a/n> "))
		line, err := u.readSingleLine()
		if err != nil {
			return approvals.Deny, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return approvals.AllowOnce, nil
		case "a", "always":
			return approvals.AllowAlways, nil
		case "n", "no", "":
			// Empty input (bare Enter) counts as "no": safer default.
			return approvals.Deny, nil
		}
		u.write(u.paint("33", "(answer y, a or n)") + "\n")
	}
}

// readSingleLine reads one physical line from the input, stripping the
// trailing newline (and optional CR, for Windows-style line endings).
func (u *TextUI) readSingleLine() (string, error) {
	line, err := u.in.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// ReadUserLine reads one logical user message. A line ending in "\" is a
// continuation marker: it is joined with the next line so long messages can
// be typed over several physical lines. A trailing "\n" is kept between
// joined lines, so the model still sees the user's paragraph breaks.
func (u *TextUI) ReadUserLine() (string, error) {
	u.FinishStream()
	var sb strings.Builder
	for {
		line, err := u.readSingleLine()
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				return sb.String(), nil
			}
			return sb.String(), err
		}
		if strings.HasSuffix(line, "\\") {
			sb.WriteString(strings.TrimSuffix(line, "\\"))
			sb.WriteString("\n")
			continue
		}
		sb.WriteString(line)
		return sb.String(), nil
	}
}
