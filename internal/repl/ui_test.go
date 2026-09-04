package repl

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func rawTestUI(input string) *TextUI {
	return &TextUI{
		in:  bufio.NewReader(strings.NewReader(input)),
		out: io.Discard,
	}
}

func fixedPrompt() func() string {
	return func() string { return "user> " }
}

// stagedReader hands out its data one stage at a time so tests can simulate
// separate keystrokes: bufio never sees the later stages until it asks for
// more, which lets a lone leading Tab arrive with an empty buffer.
type stagedReader struct {
	stages []string
}

func (s *stagedReader) Read(p []byte) (int, error) {
	if len(s.stages) == 0 {
		return 0, io.EOF
	}
	cur := s.stages[0]
	n := copy(p, cur)
	if n == len(cur) {
		s.stages = s.stages[1:]
	} else {
		s.stages[0] = cur[n:]
	}
	return n, nil
}

func stagedUI(stages ...string) *TextUI {
	return &TextUI{in: bufio.NewReader(&stagedReader{stages: stages}), out: io.Discard}
}

func TestRawReaderTabTogglesMode(t *testing.T) {
	// A lone Tab keystroke at the empty prompt toggles; text typed after it
	// is read normally.
	toggles := 0
	ui := stagedUI("\t", "hello world\n")
	line, err := ui.readRawLines(fixedPrompt(), func() { toggles++ })
	if err != nil {
		t.Fatal(err)
	}
	if line != "hello world" {
		t.Fatalf("got %q", line)
	}
	if toggles != 1 {
		t.Fatalf("Tab must fire the toggle once, got %d", toggles)
	}
}

func TestRawReaderTabMidDraftIsLiteral(t *testing.T) {
	// A tab typed or pasted while there is already draft text (or streaming
	// input) must stay content: no mode flip, no lost text.
	toggles := 0
	ui := rawTestUI("a\tb\n")
	line, err := ui.readRawLines(fixedPrompt(), func() { toggles++ })
	if err != nil {
		t.Fatal(err)
	}
	if toggles != 0 {
		t.Fatalf("mid-draft tab must not toggle, got %d", toggles)
	}
	if line != "a\tb" {
		t.Fatalf("mid-draft tab must be kept verbatim: %q", line)
	}
}

func TestRawReaderPastedTabIsLiteral(t *testing.T) {
	// Pasting tab-indented code must not flip the mode or drop text: a tab
	// with more input already buffered is content, not a toggle.
	toggles := 0
	ui := rawTestUI("first\n\tsecond\nthird\n")
	line, err := ui.readRawLines(fixedPrompt(), func() { toggles++ })
	if err != nil {
		t.Fatal(err)
	}
	if toggles != 0 {
		t.Fatalf("pasted tabs must not toggle the mode, got %d toggles", toggles)
	}
	want := "first\n\tsecond\nthird"
	if line != want {
		t.Fatalf("pasted lines must be preserved verbatim:\n got %q\nwant %q", line, want)
	}
}

func TestRawReaderBackspace(t *testing.T) {
	ui := rawTestUI("abc\x7fd\n")
	line, err := ui.readRawLines(fixedPrompt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if line != "abd" {
		t.Fatalf("backspace must erase the previous rune: %q", line)
	}
}

func TestRawReaderContinuationAndPaste(t *testing.T) {
	ui := rawTestUI("first \\\nsecond\n")
	line, err := ui.readRawLines(fixedPrompt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "first") || !strings.Contains(line, "second") {
		t.Fatalf("continuation lines must join: %q", line)
	}
}

func TestRawReaderCtrlCQuits(t *testing.T) {
	ui := rawTestUI("draft\x03")
	_, err := ui.readRawLines(fixedPrompt(), nil)
	if !errors.Is(err, errUserQuit) {
		t.Fatalf("Ctrl+C must produce errUserQuit, got %v", err)
	}
}

func TestRawReaderEscapesControlChars(t *testing.T) {
	// Control bytes (bell, and an escape sequence) must not land in the
	// draft or echo.
	ui := &TextUI{in: bufio.NewReader(bytes.NewReader([]byte{'a', 0x07, 0x1b, '[', 'A', 'b', '\n'})), out: io.Discard}
	line, err := ui.readRawLines(fixedPrompt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if line != "ab" {
		t.Fatalf("control bytes and escape sequences must be swallowed: %q", line)
	}
}

func TestRawReaderLoneEscapeSwallowsNothing(t *testing.T) {
	// A lone Escape followed by typed text must not eat the text.
	ui := rawTestUI("a\x1bb\n")
	line, err := ui.readRawLines(fixedPrompt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if line != "ab" {
		t.Fatalf("lone Escape must be dropped without eating the next key: %q", line)
	}
}

func TestRawReaderLongEscapeSequence(t *testing.T) {
	// Home/End-style CSI sequences (ESC [ 1 ~) are fully consumed: the
	// trailing '~' must not reach the draft.
	ui := rawTestUI("x\x1b[1~y\n")
	line, err := ui.readRawLines(fixedPrompt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if line != "xy" {
		t.Fatalf("long CSI sequence must be swallowed whole: %q", line)
	}
}

func TestRawReaderPromptTracksMode(t *testing.T) {
	// The prompt getter is called on every redraw, so after a Tab toggle
	// the line shows the new mode's prompt rather than a stale one.
	var out bytes.Buffer
	calls := 0
	prompt := func() string {
		calls++
		if calls == 1 {
			return "user> "
		}
		return "plan> "
	}
	ui := &TextUI{in: bufio.NewReader(&stagedReader{stages: []string{"\t", "hello\n"}}), out: &out}
	line, err := ui.readRawLines(prompt, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if line != "hello" {
		t.Fatalf("got %q", line)
	}
	text := out.String()
	if !strings.Contains(text, "user> ") {
		t.Fatalf("first draw must show the build prompt: %q", text)
	}
	if !strings.Contains(text, "plan> ") {
		t.Fatalf("redraw after Tab must show the plan prompt: %q", text)
	}
}
