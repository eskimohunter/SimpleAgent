package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type Audit struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func New(path string) (*Audit, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{f: f, w: bufio.NewWriter(f)}, nil
}

func (a *Audit) Log(kind string, data map[string]any) error {
	if a == nil {
		return nil
	}
	evt := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind}
	for k, v := range data {
		evt[k] = v
	}
	line, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.w.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := a.w.Flush(); err != nil {
		return fmt.Errorf("audit flush: %w", err)
	}
	return nil
}

func (a *Audit) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.w.Flush()
	return a.f.Close()
}
