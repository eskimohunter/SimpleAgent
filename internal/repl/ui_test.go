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
	line, err := ui.readRawLines(fixedPrompt(), func() { toggles++ }, nil)
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
	line, err := ui.readRawLines(fixedPrompt(), func() { toggles++ }, nil)
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
	line, err := ui.readRawLines(fixedPrompt(), func() { toggles++ }, nil)
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
	line, err := ui.readRawLines(fixedPrompt(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if line != "abd" {
		t.Fatalf("backspace must erase the previous rune: %q", line)
	}
}

func TestRawReaderContinuationAndPaste(t *testing.T) {
	ui := rawTestUI("first \\\nsecond\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "first") || !strings.Contains(line, "second") {
		t.Fatalf("continuation lines must join: %q", line)
	}
}

func TestRawReaderCtrlCQuits(t *testing.T) {
	ui := rawTestUI("draft\x03")
	_, err := ui.readRawLines(fixedPrompt(), nil, nil)
	if !errors.Is(err, errUserQuit) {
		t.Fatalf("Ctrl+C must produce errUserQuit, got %v", err)
	}
}

func TestRawReaderEscapesControlChars(t *testing.T) {
	// Control bytes (bell, and an escape sequence) must not land in the
	// draft or echo.
	ui := &TextUI{in: bufio.NewReader(bytes.NewReader([]byte{'a', 0x07, 0x1b, '[', 'A', 'b', '\n'})), out: io.Discard}
	line, err := ui.readRawLines(fixedPrompt(), nil, nil)
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
	line, err := ui.readRawLines(fixedPrompt(), nil, nil)
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
	line, err := ui.readRawLines(fixedPrompt(), nil, nil)
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
	line, err := ui.readRawLines(prompt, func() {}, nil)
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

// menuUI builds a color-enabled reader (the /-menu only renders with color)
// whose provider filters a fixed entry list by prefix.
func menuUI(stages ...string) *TextUI {
	return &TextUI{
		in:    bufio.NewReader(&stagedReader{stages: stages}),
		out:   io.Discard,
		color: true,
		tty:   true,
	}
}

// menuFor adapts a plain string entry list into the menuEntry provider
// (nothing confirm-guarded unless requested).
func menuFor(entries []string) func(typed string) []menuEntry {
	return func(typed string) []menuEntry {
		var out []menuEntry
		for _, e := range entries {
			if strings.HasPrefix(e, typed) {
				out = append(out, menuEntry{line: e})
			}
		}
		return out
	}
}

func TestRawMenuEnterExecutesFirst(t *testing.T) {
	entries := []string{"/alpha", "/beta", "/gamma"}
	ui := menuUI("/", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/alpha" {
		t.Fatalf("Enter must execute the first entry, got %q", line)
	}
}

func TestRawMenuArrowsSelect(t *testing.T) {
	entries := []string{"/alpha", "/beta", "/gamma"}
	ui := menuUI("/", "\x1b[B", "\x1b[B", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/gamma" {
		t.Fatalf("Down Down Enter must select the third entry, got %q", line)
	}
}

func TestRawMenuArrowWrapAround(t *testing.T) {
	entries := []string{"/alpha", "/beta", "/gamma"}
	ui := menuUI("/", "\x1b[A", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/gamma" {
		t.Fatalf("Up from the first entry must wrap to the last, got %q", line)
	}
}

func TestRawMenuTypingFilters(t *testing.T) {
	entries := []string{"/alpha", "/beta", "/gamma"}
	ui := menuUI("/", "b", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/beta" {
		t.Fatalf("typing /b must filter and execute /beta, got %q", line)
	}
}

func TestRawMenuTabAcceptsHighlight(t *testing.T) {
	entries := []string{"/alpha", "/beta", "/gamma"}
	ui := menuUI("/", "\x1b[B", "\t", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/beta" {
		t.Fatalf("Tab must accept the highlighted entry into the line, got %q", line)
	}
}

func TestRawMenuEscClosesMenu(t *testing.T) {
	entries := []string{"/alpha", "/beta", "/gamma"}
	// Esc closes the menu; the following Enter submits the raw draft
	// instead of executing the top entry.
	ui := menuUI("/", "\x1b", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/" {
		t.Fatalf("Esc must close the menu and keep the draft, got %q", line)
	}
}

func TestRawMenuNoMatchFallsThrough(t *testing.T) {
	entries := []string{"/alpha", "/beta", "/gamma"}
	ui := menuUI("/zzz\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/zzz" {
		t.Fatalf("non-matching slash text must submit normally, got %q", line)
	}
}

func TestRawMenuHiddenWithoutColor(t *testing.T) {
	// Without color the menu must never render or consume input: "/"
	// behaves like plain text.
	calls := 0
	ui := &TextUI{in: bufio.NewReader(strings.NewReader("/x\n")), out: io.Discard}
	line, err := ui.readRawLines(fixedPrompt(), nil, func(typed string) []menuEntry {
		calls++
		return []menuEntry{{line: "/xx"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if line != "/x" {
		t.Fatalf("no-color terminals must type slash lines normally, got %q", line)
	}
	if calls != 0 {
		t.Fatalf("menu provider must not run without color, got %d calls", calls)
	}
}

func TestRawMenuEnterExactMatchSubmits(t *testing.T) {
	// Typing the complete command and pressing Enter must run it (submit of
	// the full text dispatches the identical line).
	entries := []string{"/beta"}
	ui := menuUI("/", "b", "e", "t", "a", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/beta" {
		t.Fatalf("full command + Enter must run it, got %q", line)
	}
}

func TestRawMenuConfirmGuardBlocksPartialExit(t *testing.T) {
	// A stray Enter while "/e" is only a prefix of the destructive /exit
	// must submit the draft (unknown-command error), NOT quit.
	entries := []menuEntry{{line: "/exit", confirm: true}}
	ui := menuUI("/", "e", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, func(string) []menuEntry { return entries })
	if err != nil {
		t.Fatal(err)
	}
	if line != "/e" {
		t.Fatalf("guarded command must not fire on a partial prefix, got %q", line)
	}
}

func TestRawMenuConfirmGuardAllowsArrows(t *testing.T) {
	// Explicit navigation with arrows makes the guarded command executable.
	entries := []menuEntry{{line: "/exit", confirm: true}}
	ui := menuUI("/", "e", "\x1b[B", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, func(string) []menuEntry { return entries })
	if err != nil {
		t.Fatal(err)
	}
	if line != "/exit" {
		t.Fatalf("arrow selection must allow the guarded command, got %q", line)
	}
}

func TestRawMenuEscLatches(t *testing.T) {
	// Esc closes the menu for the whole line: typing more must not reopen
	// it, so the final Enter submits the raw draft instead of a candidate.
	entries := []string{"/beta", "/bogus"}
	ui := menuUI("/", "\x1b", "b", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if err != nil {
		t.Fatal(err)
	}
	if line != "/b" {
		t.Fatalf("Esc must keep the menu closed for the line, got %q", line)
	}
}

func TestRawMenuCtrlCWithMenuOpen(t *testing.T) {
	entries := []string{"/alpha", "/beta"}
	ui := menuUI("/", "\x03")
	_, err := ui.readRawLines(fixedPrompt(), nil, menuFor(entries))
	if !errors.Is(err, errUserQuit) {
		t.Fatalf("Ctrl+C with the menu open must quit, got %v", err)
	}
}

func TestRawArrowsIgnoredWithoutMenu(t *testing.T) {
	// Arrow keys with no menu visible are inert and do not disturb typing.
	ui := menuUI("\x1b[B", "x", "\n")
	line, err := ui.readRawLines(fixedPrompt(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if line != "x" {
		t.Fatalf("arrow keys must be ignored without a menu, got %q", line)
	}
}

func TestSlashMenuFilter(t *testing.T) {
	cases := []struct {
		typed string
		want  []string
	}{
		{"/", []string{"/help", "/new", "/approvals", "/mode", "/exit"}},
		{"/m", []string{"/mode"}},
		{"/ex", []string{"/exit"}},
		{"/EX", []string{"/exit"}},
		{"/zzz", nil},
	}
	for _, c := range cases {
		got := slashMenu(c.typed)
		if len(got) != len(c.want) {
			t.Errorf("slashMenu(%q) = %v, want %v", c.typed, got, c.want)
			continue
		}
		for i := range got {
			if got[i].line != c.want[i] {
				t.Errorf("slashMenu(%q) = %v, want %v", c.typed, got, c.want)
				break
			}
		}
	}
	// Destructive commands must be confirm-guarded in the real table.
	for _, c := range slashMenu("/e") {
		if c.line == "/exit" && !c.confirm {
			t.Fatal("/exit must be confirm-guarded")
		}
	}
	for _, c := range slashMenu("/n") {
		if c.line == "/new" && !c.confirm {
			t.Fatal("/new must be confirm-guarded")
		}
	}
}
