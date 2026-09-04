// Package approvals decides whether a shell command may run without (or
// against) the interactive prompt. Three rule sources feed the decision:
//   - the config allowlist (exact commands or "prefix*" rules) and the
//     persisted "always" approvals, which AUTO-APPROVE matching commands;
//   - the config denylist (same exact / "prefix*" syntax), which
//     HARD-BLOCKS matching commands: a denied command never runs, not even
//     if the allowlist or a human "always" answer would otherwise permit
//     it. Deny matching is case-insensitive, because the default Windows
//     shell (PowerShell) is case-insensitive.
//
// The engine checks the denylist first, then the allowlist, then asks the
// human (see agent.Engine.approveCommand). The file tools never consult this
// package - they are contained by code.
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

// Manager holds the allow side (config allowlist + persisted "always"
// approvals) and the deny side (config denylist) plus the persistence
// bookkeeping for the allow side.
type Manager struct {
	persist bool // whether "always" approvals survive restarts
	allow   *ruleSet
	deny    *ruleSet
	file    string // where the persisted store lives (approvals.json)
}

// ruleSet is one side of the rules (allow or deny): a set of exact
// normalized commands plus a sorted list of prefix rules. foldCase makes
// matching case-insensitive, which the deny side uses because the default
// Windows shell is case-insensitive ("curl" and "CURL" are the same
// command there).
type ruleSet struct {
	exact    map[string]struct{}
	prefixes []string
	foldCase bool
}

// newRuleSet creates an empty rule set with the requested case behavior.
func newRuleSet(foldCase bool) *ruleSet {
	return &ruleSet{exact: map[string]struct{}{}, foldCase: foldCase}
}

// add parses one config entry into the set: an exact rule, or a prefix
// rule when the entry ends in "*". Blank entries and a bare "*" are
// ignored (they would approve or block every command).
func (rs *ruleSet) add(entry string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return
	}
	// Both the exact body and the prefix body are whitespace-normalized, so
	// a rule written with runs of spaces/tabs ("git  status", "rm  -rf  /")
	// matches the normalized form every incoming command is compared as.
	if strings.HasSuffix(entry, "*") {
		// TrimSpace again in case whitespace sits between the body and "*".
		p := strings.TrimSpace(strings.TrimSuffix(normalize(entry), "*"))
		if p != "" {
			rs.prefixes = append(rs.prefixes, rs.key(p))
		}
	} else {
		rs.exact[rs.key(normalize(entry))] = struct{}{}
	}
}

// rememberExact records one exact command without prefix parsing. Used for
// "always" approvals, which are always stored as exact commands.
func (rs *ruleSet) rememberExact(cmd string) {
	c := normalize(cmd)
	if c == "" {
		return
	}
	if _, ok := rs.exact[rs.key(c)]; !ok {
		rs.exact[rs.key(c)] = struct{}{}
	}
}

// key folds the case when the set is case-insensitive.
func (rs *ruleSet) key(s string) string {
	if rs.foldCase {
		return strings.ToLower(s)
	}
	return s
}

// matches reports whether the normalized command matches any rule, and
// which kind of rule matched ("exact" or "prefix").
func (rs *ruleSet) matches(cmd string) (bool, string) {
	k := rs.key(normalize(cmd))
	if _, ok := rs.exact[k]; ok {
		return true, "exact"
	}
	for _, p := range rs.prefixes {
		if strings.HasPrefix(k, p) {
			return true, "prefix"
		}
	}
	return false, ""
}

// finish sorts the prefixes; call once all entries have been added so
// prefix scans and listings are deterministic.
func (rs *ruleSet) finish() {
	sort.Strings(rs.prefixes)
}

// snapshot returns the rules sorted for display. The prefix slice is
// copied so callers cannot mutate the set through it. Rules in a
// case-insensitive set appear in their canonical lower-cased form.
func (rs *ruleSet) snapshot() (exact, prefixes []string) {
	for c := range rs.exact {
		exact = append(exact, c)
	}
	sort.Strings(exact)
	return exact, append([]string(nil), rs.prefixes...)
}

// store is the on-disk JSON shape of the persisted "always" approvals.
type store struct {
	Version  int      `json:"version"`
	Commands []string `json:"commands"`
}

// New builds a Manager: it loads the persisted "always" approvals (if
// persist is on and the store file exists), folds in the config allowlist,
// and installs the config denylist. Allowlist rules may be exact commands
// or "*"-suffix prefixes; denylist rules use the same syntax but match
// case-insensitively and are never persisted - they live only for this
// configuration's lifetime.
func New(storeFile string, persist bool, allowlist, denylist []string) (*Manager, error) {
	m := &Manager{
		persist: persist,
		allow:   newRuleSet(false),
		deny:    newRuleSet(true),
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
				m.allow.rememberExact(c)
			}
		}
	}
	for _, entry := range allowlist {
		m.allow.add(entry)
	}
	for _, entry := range denylist {
		m.deny.add(entry)
	}
	m.allow.finish()
	m.deny.finish()
	return m, nil
}

// normalize canonicalizes a command for comparison: "git  status" and
// "git status" are the same command, so runs of whitespace collapse to a
// single space. This keeps rules effective across spacing differences.
// Case folding is NOT part of normalization - it is decided per rule set
// (see ruleSet.key): commands are case sensitive on Unix, so only the
// deny side folds case.
func normalize(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}

// Allowed reports whether cmdline is auto-approved (config allowlist or
// persisted "always") and why ("exact" or "prefix"). The denylist is NOT
// consulted here - the engine checks Denied first, so a denied command can
// never be auto-approved.
func (m *Manager) Allowed(cmd string) (bool, string) {
	return m.allow.matches(cmd)
}

// Denied reports whether cmdline is hard-blocked by the config denylist
// and which rule kind matched ("exact" or "prefix"). Matching is
// case-insensitive.
func (m *Manager) Denied(cmd string) (bool, string) {
	return m.deny.matches(cmd)
}

// Remember stores a command the user approved with "always". It is written
// to disk only when persistence is enabled; in-memory it always applies for
// the rest of the session.
func (m *Manager) Remember(cmd string) error {
	m.allow.rememberExact(cmd)
	if !m.persist || m.file == "" {
		return nil
	}
	return m.save()
}

// save writes the store atomically: it writes a temp file next to the real
// one and renames it over the target. A crash mid-write can only leave the
// temp file behind, never a half-written approvals.json.
func (m *Manager) save() error {
	exact, _ := m.allow.snapshot()
	data, err := json.MarshalIndent(store{Version: 1, Commands: exact}, "", "  ")
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

// List returns the current allow rules sorted, split for display by the
// /approvals command: exact commands and prefix rules.
func (m *Manager) List() (exact []string, prefixes []string) {
	return m.allow.snapshot()
}

// DenyList returns the config denylist rules sorted, split for display:
// exact commands and prefix rules (shown lower-cased).
func (m *Manager) DenyList() (exact []string, prefixes []string) {
	return m.deny.snapshot()
}

// DirName is a helper for callers that need the approvals file path inside
// a harness state directory.
func DirName(stateDir string) string {
	return filepath.Join(stateDir, "approvals.json")
}
