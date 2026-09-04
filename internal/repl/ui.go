package repl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"simpleagent/internal/approvals"
)

// errUserQuit is returned by the interactive reader when the user presses
// Ctrl+C at the idle prompt (mirrors the cooked-mode signal behavior).
var errUserQuit = errors.New("user quit")

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
	tty       bool // both stdin and stdout are terminals (raw Tab input works)
}

// NewTextUI wraps an input reader and output writer. When writing to a real
// terminal it enables Windows console support (UTF-8 + ANSI colors) and
// turns on ANSI color unless the NO_COLOR environment variable is set.
func NewTextUI(in io.Reader, out io.Writer) *TextUI {
	if out == os.Stdout {
		configureConsole()
	}
	color := false
	tty := false
	if f, ok := out.(*os.File); ok {
		color = isTTY(f) && os.Getenv("NO_COLOR") == ""
	}
	if f, ok := in.(*os.File); ok && isTTY(f) {
		if of, ok := out.(*os.File); ok && isTTY(of) {
			tty = true
		}
	}
	return &TextUI{in: bufio.NewReader(in), out: out, color: color, tty: tty}
}

// Interactive reports whether input comes from a real terminal (raw-mode
// key handling such as the Tab toggle is only possible then).
func (u *TextUI) Interactive() bool {
	return u.tty
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

// ReadUserInteractive reads one logical user message from a TTY in raw
// (character) mode. While typing: a Tab at the empty prompt calls onTab
// (the mode toggle), printable characters echo, Backspace erases, Enter
// submits, and Ctrl+C returns errUserQuit (the caller exits). Lines joined
// with a trailing "\" or pasted in one burst are merged like ReadUserLine.
// prompt is called for every redraw so the visible prefix follows mode
// toggles immediately.
func (u *TextUI) ReadUserInteractive(prompt func() string, onTab func()) (string, error) {
	u.FinishStream()
	restore, err := enterRawMode()
	if err != nil {
		// Raw input unavailable (unsupported platform, not a console):
		// degrade to the cooked reader on a fresh line.
		fmt.Fprintln(u.out)
		return u.ReadUserLine()
	}
	defer restore()
	return u.readRawLines(prompt, onTab)
}

// readRawLines drives the character-mode input loop and merges continuation
// and pasted lines into one logical message (same rules as ReadUserLine).
func (u *TextUI) readRawLines(prompt func() string, onTab func()) (string, error) {
	var sb strings.Builder
	for {
		line, err := u.readRawLine(prompt, onTab)
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				return sb.String(), nil
			}
			if err == errUserQuit {
				return "", err
			}
			return sb.String(), err
		}
		joined := false
		if strings.HasSuffix(line, "\\") {
			sb.WriteString(strings.TrimSuffix(line, "\\"))
			sb.WriteString("\n")
			joined = true
		} else {
			sb.WriteString(line)
		}
		// Paste heuristic: more input already buffered behind the Enter key
		// means the user pasted several lines; treat them as one message.
		if u.in.Buffered() > 0 {
			sb.WriteString("\n")
			joined = true
		}
		if joined {
			fmt.Fprintln(u.out)
			continue
		}
		return sb.String(), nil
	}
}

// readRawLine reads one physical line in raw mode, echoing keys itself. The
// prompt is drawn first and redrawn after Tab toggles. A Tab toggles the
// mode only when pressed at the empty prompt with nothing buffered (a real
// keystroke, not paste); anywhere else it is literal text, so pasting
// tab-indented code can never flip the mode or drop the draft.
func (u *TextUI) readRawLine(prompt func() string, onTab func()) (string, error) {
	draft := []rune{}
	draw := func() {
		line := prompt() + string(draft)
		if u.color {
			// ANSI erase-to-end-of-line; VT processing is enabled exactly
			// when colors are on.
			fmt.Fprintf(u.out, "\r\x1b[K%s", line)
		} else {
			// No ANSI (NO_COLOR / legacy console): overwrite the line with
			// spaces instead of emitting escape codes.
			fmt.Fprintf(u.out, "\r%s\r%s", strings.Repeat(" ", len(line)), line)
		}
	}
	draw()
	for {
		r, _, err := u.in.ReadRune()
		if err != nil {
			return string(draft), err
		}
		switch r {
		case '\t':
			if len(draft) == 0 && u.in.Buffered() == 0 {
				// A genuine Tab keystroke at the empty prompt toggles the
				// mode. Tabs typed or pasted into a message (mid-line, or
				// any tab while input is still streaming in) are content.
				if onTab != nil {
					onTab()
				}
				draw()
			} else {
				draft = append(draft, r)
				u.write(string(r))
			}
		case '\r', '\n':
			fmt.Fprintln(u.out)
			return string(draft), nil
		case 0x7f, '\b':
			if len(draft) > 0 {
				draft = draft[:len(draft)-1]
				draw()
			}
		case 0x03:
			// Ctrl+C reached us as a byte because raw mode disabled the
			// terminal's signal processing. Same meaning as at the idle
			// cooked prompt: quit.
			return "", errUserQuit
		case 0x1b:
			// Escape: swallow a real escape sequence (CSI/SS3) so its bytes
			// are neither echoed nor typed. A lone Escape is put back and
			// discarded harmlessly on the next read: nothing is swallowed
			// that the user typed deliberately.
			next, _, rerr := u.in.ReadRune()
			if rerr != nil {
				return string(draft), rerr
			}
			switch next {
			case '[': // CSI: ESC [ ... final byte 0x40-0x7e
				for i := 0; i < 16; i++ {
					final, _, rerr := u.in.ReadRune()
					if rerr != nil {
						return string(draft), rerr
					}
					if final >= 0x40 && final <= 0x7e {
						break
					}
				}
			case 'O': // SS3 (F1-F4): ESC O P
				_, _, _ = u.in.ReadRune()
			default:
				// Not a sequence: put the rune back so it reaches the
				// draft on the next iteration (lone Escape is dropped).
				_ = u.in.UnreadRune()
			}
		default:
			if r >= ' ' {
				draft = append(draft, r)
				u.write(string(r))
			}
			// Other control characters are ignored without echo.
		}
	}
}
