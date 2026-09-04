package approvals

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Decision int

const (
	Deny Decision = iota
	AllowOnce
	AllowAlways
)

type Manager struct {
	persist  bool
	exact    map[string]struct{}
	prefixes []string
	file     string
}

type store struct {
	Version  int      `json:"version"`
	Commands []string `json:"commands"`
}

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

func normalize(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}

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

func (m *Manager) List() (exact []string, prefixes []string) {
	for c := range m.exact {
		exact = append(exact, c)
	}
	sort.Strings(exact)
	return exact, append([]string(nil), m.prefixes...)
}

func DirName(stateDir string) string {
	return filepath.Join(stateDir, "approvals.json")
}
