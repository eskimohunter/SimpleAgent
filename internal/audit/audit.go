// Package audit provides the append-only audit trail. Every meaningful
// event - user messages, model replies, tool calls and their results,
// approval decisions, command outcomes - is written as one JSON line to
// .agent/audit.jsonl, so there is a durable record of what the agent did and
// why (matching the threat model in docs/security.md).
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Audit appends timestamped JSON events to a file. The mutex serializes
// writers because events arrive from several goroutines at once (engine
// turns, stream readers). A bufio.Writer is used for efficiency, but Log
// flushes after every event so nothing survives only in the buffer.
type Audit struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// New opens (creating if needed) the log file in append mode with
// owner-only permissions (0o600): the trail may contain command lines the
// user typed or approved, which can be sensitive.
func New(path string) (*Audit, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{f: f, w: bufio.NewWriter(f)}, nil
}

// Log appends one event. kind is a stable machine-readable tag such as
// "tool_call" or "approval_denied"; data is merged into the event after the
// timestamp and kind. Events are flushed immediately (see Audit) so a crash
// loses at most the event being written.
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

// Close flushes pending data and closes the file. All methods tolerate a
// nil receiver so callers can pass an optional audit around safely.
func (a *Audit) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.w.Flush()
	return a.f.Close()
}
