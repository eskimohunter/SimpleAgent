// Package approvals decides which shell commands may run without an
// interactive prompt. Two independent sources feed the decision:
//   - the config allowlist (prefix rules such as "git status*"), and
//   - commands the user approved with "always", persisted in
//     .agent/approvals.json across restarts.
//
// Approval is the human-gate for run_command (see agent.Engine.approveCommand);
// the file tools never consult this package - they are contained by code.
package approvals

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Decision is the user's answer at an approval prompt.
type Decision int

const (
	Deny        Decision = iota // run the command? no
	AllowOnce                   // yes, this one time
	AllowAlways                 // yes, and remember it for future turns
)

// Manager holds the two approval sources and the persistence bookkeeping.
type Manager struct {
	persist  bool // whether "always" approvals survive restarts
	exact    map[string]struct{}
	prefixes []string
	file     string // where the persisted store lives (approvals.json)
}

// store is the on-disk JSON shape of the persisted approvals.
type store struct {
	Version  int      `json:"version"`
	Commands []string `json:"commands"`
}

// New builds a Manager: it loads the persisted store (if persist is on and
// the file exists), then folds the config allowlist in, splitting each entry
// into an exact command or a prefix rule (entries ending in "*").
func New(storeFile string, persist bool, allowlist []string) (*Manager, error) {
	m := &Manager{
		persist: persist,
		exact:   map[string]struct{}{},
		file:    storeFile,
	}
	if persist && storeFile != "" {
		if data, err := os.ReadFile(storeFile); err == nil {
			var s store
			if err := json.Unmarshal(data, &s); err != nil {
				// A corrupt approvals file is a hard error: silently
				// discarding it could auto-approve or forget commands
				// without the user noticing.
				return nil, fmt.Errorf("approvals store %s is corrupt: %w", storeFile, err)
			}
			for _, c := range s.Commands {
				m.exact[normalize(c)] = struct{}{}
			}
		}
	}
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.HasSuffix(entry, "*") {
			p := strings.TrimSuffix(entry, "*")
			if p != "" {
				m.prefixes = append(m.prefixes, p)
			}
		} else {
			m.exact[normalize(entry)] = struct{}{}
		}
	}
	sort.Strings(m.prefixes)
	return m, nil
}

// normalize canonicalizes a command for comparison: "git  status" and
// "git status" are the same command, so runs of whitespace collapse to a
// single space. This keeps approvals effective across spacing differences
// without making them dangerous (no case folding - commands are case
// sensitive on Unix).
func normalize(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}

// Allowed reports whether cmdline is auto-approved and why ("exact" for a
// literal match, "prefix" for a prefix-rule match, "" if not allowed).
func (m *Manager) Allowed(cmd string) (bool, string) {
	c := normalize(cmd)
	if _, ok := m.exact[c]; ok {
		return true, "exact"
	}
	for _, p := range m.prefixes {
		if strings.HasPrefix(c, p) {
			return true, "prefix"
		}
	}
	return false, ""
}

// Remember stores a command the user approved with "always". It is written
// to disk only when persistence is enabled; in-memory it always applies for
// the rest of the session.
func (m *Manager) Remember(cmd string) error {
	c := normalize(cmd)
	if c == "" {
		return nil
	}
	if _, ok := m.exact[c]; !ok {
		m.exact[c] = struct{}{}
	}
	if !m.persist || m.file == "" {
		return nil
	}
	return m.save()
}

// save writes the store atomically: it writes a temp file next to the real
// one and renames it over the target. A crash mid-write can only leave the
// temp file behind, never a half-written approvals.json.
func (m *Manager) save() error {
	cmds := make([]string, 0, len(m.exact))
	for c := range m.exact {
		cmds = append(cmds, c)
	}
	sort.Strings(cmds)
	data, err := json.MarshalIndent(store{Version: 1, Commands: cmds}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}

// List returns the current rules sorted, split for display by the
// /approvals command: exact commands and prefix rules. The prefix slice is
// copied so callers cannot mutate the manager through it.
func (m *Manager) List() (exact []string, prefixes []string) {
	for c := range m.exact {
		exact = append(exact, c)
	}
	sort.Strings(exact)
	return exact, append([]string(nil), m.prefixes...)
}

// DirName is a helper for callers that need the approvals file path inside
// a harness state directory.
func DirName(stateDir string) string {
	return filepath.Join(stateDir, "approvals.json")
}
