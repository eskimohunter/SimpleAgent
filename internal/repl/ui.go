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

type TextUI struct {
	mu        sync.Mutex
	in        *bufio.Reader
	out       io.Writer
	color     bool
	streaming bool
}

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

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (u *TextUI) paint(code, s string) string {
	if !u.color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *TextUI) write(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprint(u.out, s)
}

func (u *TextUI) Notice(kind, msg string) {
	if kind == "out" || kind == "err" {
		var prefix string
		switch kind {
		case "err":
			prefix = u.paint("31", msg)
		default:
			prefix = u.paint("2", msg)
		}
		u.write(prefix)
		return
	}
	var text string
	switch kind {
	case "tool":
		text = u.paint("36", "» "+msg) + "\n"
	case "run":
		text = u.paint("1;36", "$ "+msg) + "\n"
	case "deny":
		text = u.paint("33", "denied: "+msg) + "\n"
	case "warn":
		text = u.paint("33", msg) + "\n"
	default:
		text = msg + "\n"
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		fmt.Fprintln(u.out)
		u.streaming = false
	}
	fmt.Fprint(u.out, text)
}

func (u *TextUI) StreamText(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.streaming = true
	fmt.Fprint(u.out, s)
}

func (u *TextUI) Info(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		fmt.Fprintln(u.out)
		u.streaming = false
	}
	fmt.Fprint(u.out, u.paint("2", s), "\n")
}

func (u *TextUI) Error(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		fmt.Fprintln(u.out)
		u.streaming = false
	}
	fmt.Fprint(u.out, u.paint("31", s), "\n")
}

func (u *TextUI) FinishStream() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.streaming {
		fmt.Fprintln(u.out)
		u.streaming = false
	}
}

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
			return approvals.Deny, nil
		}
		u.write(u.paint("33", "(answer y, a or n)") + "\n")
	}
}

func (u *TextUI) readSingleLine() (string, error) {
	line, err := u.in.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

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
