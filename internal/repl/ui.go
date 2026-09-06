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
		line, err := u.readPrompt(u.paint("33", "y/a/n> "))
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

// readPrompt reads one short answer from the user (approval and confirm
// prompts). On a real terminal it switches to raw mode and echoes keys
// itself, so the read works regardless of the terminal's line discipline:
// a cooked read depends on CR->LF input translation (termios ICRNL) to
// even receive the Enter key - with ICRNL off, canonical mode treats CR
// as plain data, never as a line terminator, and the read hangs forever
// while the ECHOCTL'd "^M" is what the user sees. Raw mode delivers every
// key as it is pressed and the CR becomes a plain byte we can match.
//
// Handled keys: Enter (CR or LF) submits, Backspace erases, Ctrl+C
// returns errUserQuit. Only printable characters echo, so no control
// sequences can leak into the transcript. The terminal is restored before
// returning, exactly like ReadUserInteractive does. On non-TTY input
// (pipes, CI) the prompt is printed and the rune-based readSingleLine is
// used, preserving the cooked behavior.
func (u *TextUI) readPrompt(prompt string) (string, error) {
	if !u.tty {
		u.write(prompt)
		return u.readSingleLine()
	}
	restore, err := enterRawMode()
	if err != nil {
		// Raw input unavailable (unsupported platform, not a console):
		// degrade to the cooked reader on a fresh line.
		fmt.Fprintln(u.out)
		u.write(prompt)
		return u.readSingleLine()
	}
	defer restore()
	u.write(prompt)
	var sb strings.Builder
	for {
		r, _, err := u.in.ReadRune()
		if err != nil {
			return sb.String(), err
		}
		switch r {
		case '\r', '\n':
			// Enter submits the answer.
			fmt.Fprintln(u.out)
			return sb.String(), nil
		case 0x7f, '\b':
			// Backspace erases one rune (echoing the erase sequence).
			rr := []rune(sb.String())
			if len(rr) > 0 {
				rr = rr[:len(rr)-1]
				sb.Reset()
				sb.WriteString(string(rr))
				u.write("\b \b")
			}
		case 0x03:
			// Ctrl+C: same meaning as at the idle raw prompt - quit-ish.
			fmt.Fprintln(u.out)
			return "", errUserQuit
		default:
			if r >= ' ' {
				sb.WriteRune(r)
				u.write(string(r))
			}
		}
	}
}

// readSingleLine reads one physical line from the input, terminating on a
// bare CR, a bare LF, or a CRLF pair. Both line endings are accepted because
// the caller's CR->LF input translation (termios ICRNL) is not guaranteed:
// a raw-mode toggle, a terminal started with "stty -icrnl", or a CRLF
// console would otherwise leave the "\n" read hanging forever (the CR never
// becomes a newline). The line ending is stripped from the result.
func (u *TextUI) readSingleLine() (string, error) {
	var sb strings.Builder
	for {
		r, _, err := u.in.ReadRune()
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				return sb.String(), nil
			}
			return sb.String(), err
		}
		if r == '\n' {
			return sb.String(), nil
		}
		if r == '\r' {
			// CRLF: consume the LF that follows (if any) so it cannot be
			// misread as an empty next line.
			if next, _, err := u.in.ReadRune(); err == nil && next != '\n' {
				_ = u.in.UnreadRune()
			}
			return sb.String(), nil
		}
		sb.WriteRune(r)
	}
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

// menuEntry is one entry of the /-command menu. confirm marks commands
// whose accidental execution is costly (/exit quits, /new wipes history):
// they only run when the user typed the full command or explicitly moved
// the selection with arrows, never from a stray Enter on a partial prefix.
type menuEntry struct {
	line    string
	confirm bool
}

// menuSelection is returned by the raw reader when the user executes a
// command from the /-menu with Enter: its line is what the REPL should run.
type menuSelection struct {
	line string
}

func (m menuSelection) Error() string {
	return "menu selection: " + m.line
}

// ReadUserInteractive reads one logical user message from a TTY in raw
// (character) mode. While typing: a Tab at the empty prompt calls onTab
// (the mode toggle), printable characters echo, Backspace erases, Enter
// submits, and Ctrl+C returns errUserQuit (the caller exits). Lines joined
// with a trailing "\" or pasted in one burst are merged like ReadUserLine.
// prompt is called for every redraw so the visible prefix follows mode
// toggles immediately.
//
// When menu is non-nil and the draft starts with "/", the entries it
// returns are shown below the input: Up/Down move the highlight, Enter
// executes the highlighted entry (returned as menuSelection), Tab accepts
// it into the line, and typing filters. Esc dismisses the menu for the rest
// of the line (typing no longer reopens it). Entries marked confirm only
// run when the full command was typed or the selection was moved with
// arrows, so a stray Enter on "/e" cannot accidentally quit. The menu is
// only rendered on color terminals; elsewhere "/" lines behave as plain
// text (the REPL still dispatches them on submit).
func (u *TextUI) ReadUserInteractive(prompt func() string, onTab func(), menu func(typed string) []menuEntry) (string, error) {
	u.FinishStream()
	restore, err := enterRawMode()
	if err != nil {
		// Raw input unavailable (unsupported platform, degraded console):
		// degrade to the cooked reader on a fresh line. The prompt must be
		// drawn here - the cooked reader never draws one, and on Windows a
		// "failed" raw-mode setup can still have applied raw mode (no echo),
		// so the user needs the prompt text up front.
		fmt.Fprintln(u.out)
		u.write(prompt())
		return u.ReadUserLine()
	}
	defer restore()
	return u.readRawLines(prompt, onTab, menu)
}

// readRawLines drives the character-mode input loop and merges continuation
// and pasted lines into one logical message (same rules as ReadUserLine).
func (u *TextUI) readRawLines(prompt func() string, onTab func(), menu func(typed string) []menuEntry) (string, error) {
	var sb strings.Builder
	for {
		// The command menu only applies to the FIRST physical line: once a
		// message has content, a leading "/" on a later line is text.
		line, err := u.readRawLine(prompt, onTab, menu, sb.Len() == 0)
		if err != nil {
			var sel menuSelection
			if errors.As(err, &sel) {
				return sel.line, nil
			}
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
// prompt is drawn first and redrawn after every change. A Tab toggles the
// mode only when pressed at the empty prompt with nothing buffered (a real
// keystroke, not paste); anywhere else it is literal text, so pasting
// tab-indented code can never flip the mode or drop the draft. See
// ReadUserInteractive for the "/"-menu behavior enabled by menuAllowed.
func (u *TextUI) readRawLine(prompt func() string, onTab func(), menu func(typed string) []menuEntry, menuAllowed bool) (string, error) {
	draft := []rune{}
	cands := []menuEntry{}
	sel := 0
	drawnRows := 0         // menu rows currently on screen below the input
	lastTyped := ""        // draft text the visible menu was filtered by
	menuBlocked := false   // one-shot dismiss (Tab accept)
	menuDismissed := false // Esc closed the menu for the rest of this line
	navigated := false     // the user moved the highlight with arrows

	// fmtPrintf to u.out without the mutex; we are the only writer while
	// reading a line in raw mode.
	draw := func() {
		typed := string(draft)
		// Refresh the candidate list.
		if typed != lastTyped {
			sel = 0
			lastTyped = typed
		}
		cands = nil
		if menuAllowed && u.color && menu != nil && u.in.Buffered() == 0 &&
			len(draft) > 0 && draft[0] == '/' && !menuBlocked && !menuDismissed {
			c := menu(typed)
			if len(c) > 0 {
				cands = c
			}
		}
		menuBlocked = false
		if sel >= len(cands) {
			sel = 0
		}
		if !u.color {
			// No ANSI (NO_COLOR / legacy console): no menu is ever drawn,
			// so a space-overwrite redraw of the input line is enough.
			line := prompt() + string(draft)
			fmt.Fprintf(u.out, "\r%s\r%s", strings.Repeat(" ", len(line)), line)
			return
		}
		// Clear the input row and whatever menu rows were drawn before,
		// moving back up, then redraw input and the current menu.
		fmt.Fprintf(u.out, "\r\x1b[K")
		for i := 0; i < drawnRows; i++ {
			fmt.Fprint(u.out, "\n\x1b[K")
		}
		if drawnRows > 0 {
			fmt.Fprintf(u.out, "\x1b[%dA", drawnRows)
		}
		fmt.Fprintf(u.out, "\r\x1b[K%s", prompt()+string(draft))
		for i, c := range cands {
			fmt.Fprint(u.out, "\n\x1b[K")
			if i == sel {
				fmt.Fprint(u.out, "\x1b[7m"+c.line+"\x1b[0m")
			} else {
				fmt.Fprint(u.out, u.paint("2", c.line))
			}
		}
		if len(cands) > 0 {
			fmt.Fprintf(u.out, "\x1b[%dA", len(cands))
		}
		drawnRows = len(cands)
	}

	// submit ends the line and leaves the cursor on a fresh row. The typed
	// input line itself stays visible (transcripts match the no-color and
	// cooked paths); only the ephemeral menu rows below it are cleared.
	submit := func() {
		if drawnRows > 0 {
			// Cursor sits at the end of the input row: go down clearing each
			// menu row, then return to the input row end.
			for i := 0; i < drawnRows; i++ {
				fmt.Fprint(u.out, "\n\x1b[K")
			}
			fmt.Fprintf(u.out, "\x1b[%dA", drawnRows)
		}
		fmt.Fprintln(u.out)
		drawnRows = 0
	}

	menuVisible := func() bool { return len(cands) > 0 }
	draw()
	for {
		r, _, err := u.in.ReadRune()
		if err != nil {
			return string(draft), err
		}
		switch r {
		case '\t':
			if menuVisible() {
				// Menu open: accept the highlighted command into the line
				// and dismiss the menu (a later Enter runs it).
				draft = []rune(cands[sel].line)
				menuBlocked = true
				draw()
			} else if len(draft) == 0 && u.in.Buffered() == 0 {
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
			if menuVisible() {
				entry := cands[sel]
				if entry.confirm && entry.line != string(draft) && !navigated {
					// Guarded (destructive) command reached only by typing a
					// partial prefix: a stray Enter must not quit or wipe
					// the session - submit the draft instead (it hits the
					// unknown-command path and the REPL keeps running).
					submit()
					return string(draft), nil
				}
				// Enter executes the highlighted command.
				submit()
				return "", menuSelection{line: entry.line}
			}
			submit()
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
			submit()
			return "", errUserQuit
		case 0x1b:
			// Escape: swallow real sequences (CSI/SS3) and interpret arrow
			// keys against the open menu. A lone Escape dismisses the menu
			// or is dropped.
			next, _, rerr := u.in.ReadRune()
			if rerr != nil {
				if rerr == io.EOF {
					// Stream ended right after ESC: treat as lone Escape.
					if menuVisible() {
						menuDismissed = true
						menuBlocked = true
						draw()
					}
					continue
				}
				return string(draft), rerr
			}
			switch next {
			case '[': // CSI: ESC [ ...
				key, _, rerr := u.in.ReadRune()
				if rerr != nil {
					if rerr == io.EOF {
						continue // truncated sequence: drop it
					}
					return string(draft), rerr
				}
				switch key {
				case 'A', 'B': // Up / Down arrow
					if menuVisible() {
						if key == 'A' {
							sel = (sel - 1 + len(cands)) % len(cands)
						} else {
							sel = (sel + 1) % len(cands)
						}
						navigated = true
						draw()
					}
				case 'C', 'D': // Right / Left: not used by the menu
				default:
					// Multi-byte sequences ([1~ etc.): consume up to the
					// final byte (0x40-0x7e).
					if key < 0x40 || key > 0x7e {
						for i := 0; i < 16; i++ {
							final, _, rerr := u.in.ReadRune()
							if rerr != nil {
								return string(draft), rerr
							}
							if final >= 0x40 && final <= 0x7e {
								break
							}
						}
					}
				}
			case 'O': // SS3 (F1-F4): ESC O P
				_, _, _ = u.in.ReadRune()
			default:
				// Whatever followed was not a sequence: put it back so the
				// user's next keystroke is processed normally.
				_ = u.in.UnreadRune()
				if menuVisible() {
					// A lone Escape closes the menu for the REST of the
					// line: typing after it must not reopen the menu, so a
					// later Enter submits the draft instead of running a
					// highlighted command.
					menuDismissed = true
					menuBlocked = true
					draw()
				}
			}
		default:
			if r >= ' ' {
				draft = append(draft, r)
				u.write(string(r))
				if len(draft) > 0 && draft[0] == '/' && menuAllowed && u.color {
					// Typing inside a slash line: refresh the menu/filter.
					draw()
				}
			}
			// Other control characters are ignored without echo.
		}
	}
}
