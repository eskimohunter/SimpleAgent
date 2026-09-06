//go:build windows

// The Windows console needs several fixes that Unix terminals get for free:
//  1. the legacy console uses a non-UTF-8 codepage, which garbles UTF-8
//     output, so we switch input and output to codepage 65001 (UTF-8);
//  2. ANSI colors are off by default, so we enable "virtual terminal
//     processing" on the stdout handle - this makes the \x1b[...m escapes
//     produced by TextUI.paint render as colors instead of garbage;
//  3. a raw-mode (line input off) console delivers keystrokes through
//     ReadFile as binary INPUT_RECORD-shaped data (event-type words, virtual
//     key/scan codes and UTF-16 code units), not as a UTF-8 byte stream - the
//     rune reader used by the UI would swallow them as control glyphs, so
//     typed text would never appear. The raw-mode reader therefore switches
//     to a stream built from ReadConsoleInputW KEY_EVENT records (consoleStream
//     below), which is deterministic on both legacy conhost and Windows
//     Terminal (ConPTY), translated into the same UTF-8 + VT byte stream a
//     Unix terminal produces.
//
// The functions are called from syscall.NewLazyDLL, which loads kernel32.dll
// on demand and lets us call Win32 APIs without cgo or external deps.
package repl

import (
	"errors"
	"io"
	"os"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

const (
	enableVirtualTerminalProcessing = 0x0004
	stdOutputHandle                 = ^uintptr(10)
	stdInputHandle                  = ^uintptr(9)

	// Console input modes cleared by enterRawMode so each key press is
	// delivered immediately instead of being line-buffered and echoed.
	enableProcessedInput = 0x0001 // Ctrl+C handled by the system (off = we see it as a byte)
	enableLineInput      = 0x0002 // Enter delivers the whole line (off = per key)
	enableEchoInput      = 0x0004 // system echo (off = we echo ourselves)
)

// ReadConsoleInputW record constants.
const (
	keyEventRecord = 0x0001

	vkBack   = 0x08
	vkTab    = 0x09
	vkReturn = 0x0D
	vkEscape = 0x1B
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28

	leftCtrlPressed  = 0x0008
	rightCtrlPressed = 0x0004
)

func configureConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procSetOutCP := kernel32.NewProc("SetConsoleOutputCP")
	procSetCP := kernel32.NewProc("SetConsoleCP")
	procGetHandle := kernel32.NewProc("GetStdHandle")
	procGetMode := kernel32.NewProc("GetConsoleMode")
	procSetMode := kernel32.NewProc("SetConsoleMode")

	// 65001 = UTF-8 codepage.
	_, _, _ = procSetOutCP.Call(65001)
	_, _, _ = procSetCP.Call(65001)

	if os.Getenv("NO_COLOR") != "" {
		return
	}
	// GetStdHandle(STD_OUTPUT_HANDLE = -11, i.e. ^uintptr(10)).
	h, _, _ := procGetHandle.Call(stdOutputHandle)
	if h == 0 || h == ^uintptr(0) {
		return
	}
	var mode uint32
	ret, _, _ := procGetMode.Call(h, uintptr(unsafe.Pointer(&mode)))
	if ret == 0 {
		return
	}
	_, _, _ = procSetMode.Call(h, uintptr(mode|enableVirtualTerminalProcessing))
}

// enterRawMode switches the console input to character mode (no line
// buffering, no echo, no system Ctrl+C processing) so the REPL can read
// single keys such as Tab. Returns a function restoring the previous mode.
func enterRawMode() (func(), error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procGetHandle := kernel32.NewProc("GetStdHandle")
	procGetMode := kernel32.NewProc("GetConsoleMode")
	procSetMode := kernel32.NewProc("SetConsoleMode")

	// GetStdHandle(STD_INPUT_HANDLE = -10, i.e. ^uintptr(9)).
	h, _, _ := procGetHandle.Call(stdInputHandle)
	if h == 0 || h == ^uintptr(0) {
		return nil, errors.New("no console input handle")
	}
	var old uint32
	ret, _, _ := procGetMode.Call(h, uintptr(unsafe.Pointer(&old)))
	if ret == 0 {
		return nil, errors.New("GetConsoleMode failed")
	}
	raw := old &^ (enableProcessedInput | enableLineInput | enableEchoInput)
	// Check the BOOL return value, not the last-error value: Win32 APIs
	// leave GetLastError() stale on success, so testing the err slot of
	// SyscallN would report failure (and take the cooked fallback) even
	// though the mode was applied - leaving the console raw with echo
	// off and no UI drawing a prompt. Same pattern as GetConsoleMode
	// above.
	ret, _, _ = procSetMode.Call(h, uintptr(raw))
	if ret == 0 {
		return nil, errors.New("SetConsoleMode failed")
	}
	return func() {
		_, _, _ = procSetMode.Call(h, uintptr(old))
	}, nil
}

// keyRecord mirrors KEY_EVENT_RECORD (16 bytes): whether the key went down,
// the repeat count, virtual key and scan codes, the Unicode character (0 for
// keys without one, e.g. arrows) and the control-key state.
type keyRecord struct {
	keyDown     int32
	repeatCount uint16
	virtualKey  uint16
	scanCode    uint16
	char        uint16
	controlKey  uint32
}

// inputRecord mirrors INPUT_RECORD (20 bytes): the event type word followed
// by the KEY_EVENT_RECORD (the largest member of the union).
type inputRecord struct {
	eventType uint16
	pad       uint16
	key       keyRecord
}

// consoleStream turns Windows console KEY_EVENT records into the UTF-8 + VT
// byte stream the raw-mode reader expects (see the package comment). It
// blocks until at least one event is available, mirroring a Unix terminal's
// delivery:
//
//	character keys (full Unicode, incl. surrogate pairs) -> UTF-8;
//	Enter/Tab/Backspace/Escape                          -> \r \t \b \x1b;
//	arrow keys         -> CSI sequences (\x1b[A etc., the form the UI's
//	                       escape parser already handles);
//	Ctrl+C (raw mode  Delivers it as a key event)      -> \x03.
//	Held keys are repeated per wRepeatCount. Key releases, mouse and
//	window-size events are skipped; batches containing no printable event
//	(e.g. a lone Shift press) are re-read.
type consoleStream struct {
	procRead *syscall.LazyProc
	h        uintptr
	pending  []byte // translated bytes awaiting a Read call
	high     uint16 // pending UTF-16 high surrogate
}

// newConsoleInput returns the translated console-key stream, or nil when raw
// input is not available (pipes, no console).
func newConsoleInput() (io.Reader, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := kernel32.NewProc("GetStdHandle").Call(stdInputHandle)
	if h == 0 || h == ^uintptr(0) {
		return nil, errors.New("no console input handle")
	}
	return &consoleStream{
		procRead: kernel32.NewProc("ReadConsoleInputW"),
		h:        h,
	}, nil
}

func (s *consoleStream) Read(dst []byte) (int, error) {
	for {
		if len(s.pending) > 0 {
			n := copy(dst, s.pending)
			s.pending = s.pending[n:]
			return n, nil
		}
		var recs [64]inputRecord
		var n uint32
		for n == 0 {
			ret, _, _ := s.procRead.Call(s.h,
				uintptr(unsafe.Pointer(&recs[0])),
				uintptr(len(recs)),
				uintptr(unsafe.Pointer(&n)))
			if ret == 0 {
				return 0, errors.New("ReadConsoleInputW failed")
			}
		}
		out := make([]byte, 0, 4096)
		for i := uint32(0); i < n; i++ {
			rec := recs[i]
			if rec.eventType != keyEventRecord || rec.key.keyDown == 0 {
				continue
			}
			ch := rec.key.char
			if ch != 0 {
				r := rune(ch)
				if ch >= 0xD800 && ch <= 0xDBFF {
					// High surrogate: hold it until the low half arrives.
					s.high = ch
					continue
				}
				if ch >= 0xDC00 && ch <= 0xDFFF {
					if s.high == 0 {
						continue // orphaned low surrogate
					}
					r = utf16.DecodeRune(rune(s.high), rune(ch))
					s.high = 0
				} else {
					s.high = 0 // drop any unpaired high surrogate
				}
				rep := int(rec.key.repeatCount)
				if rep < 1 {
					rep = 1
				}
				if rep > 16 {
					rep = 16
				}
				for j := 0; j < rep; j++ {
					out = utf8.AppendRune(out, r)
				}
				continue
			}
			// No character for this key (e.g. an arrow): map the key by
			// its virtual-key code.
			seq := s.vkeySequence(rec.key.virtualKey, rec.key.controlKey)
			if seq != nil {
				out = append(out, seq...)
			}
		}
		if len(out) > 0 {
			s.pending = out
		}
	}
}

// vkeySequence maps a virtual-key code with no Unicode character to the
// byte sequence the UI's escape parser understands.
func (s *consoleStream) vkeySequence(vk uint16, ctrl uint32) []byte {
	switch vk {
	case vkReturn:
		return []byte("\r")
	case vkTab:
		return []byte("\t")
	case vkBack:
		return []byte("\b")
	case vkEscape:
		return []byte("\x1b")
	case vkUp:
		return []byte("\x1b[A")
	case vkDown:
		return []byte("\x1b[B")
	case vkRight:
		return []byte("\x1b[C")
	case vkLeft:
		return []byte("\x1b[D")
	case 'C':
		// Ctrl+C: with ENABLE_PROCESSED_INPUT off the console delivers it
		// as a 'C' key event with the control state set.
		if ctrl&(leftCtrlPressed|rightCtrlPressed) != 0 {
			return []byte("\x03")
		}
	}
	return nil
}
