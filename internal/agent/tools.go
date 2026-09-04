package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"simpleagent/internal/model"
)

type ToolResult struct {
	Content string
	Error   error
}

func (t ToolResult) Text() string {
	if t.Error != nil {
		return "ERROR: " + t.Error.Error()
	}
	return t.Content
}

var fileTools = []model.Tool{
	model.NewTool(
		"list_files",
		"List files and directories under the given path inside the project root. "+
			"Path is relative to the project root (\"\" for the root itself); use / as separator. "+
			"Set recursive to explore subdirectories. The harness state directory .agent is hidden and protected.",
		map[string]any{
			"path":      map[string]any{"type": "string", "description": "relative directory path, empty for root"},
			"recursive": map[string]any{"type": "boolean", "description": "list recursively"},
		},
		[]string{"path"},
	),
	model.NewTool(
		"read_file",
		"Read a text file inside the project root. Output is line-numbered. "+
			"If the file exceeds the read cap, pass the 1-based offset (and optional limit, default 500) to page through it.",
		map[string]any{
			"path":   map[string]any{"type": "string", "description": "relative path, / separators"},
			"offset": map[string]any{"type": "integer", "description": "first line to show, 1-based"},
			"limit":  map[string]any{"type": "integer", "description": "max lines to show"},
		},
		[]string{"path"},
	),
	model.NewTool(
		"write_file",
		"Create or overwrite a file inside the project root (UTF-8). "+
			"Parent directories are created automatically. Use append=true to add to an existing file. "+
			"Never use this tool outside the project root or inside .agent.",
		map[string]any{
			"path":    map[string]any{"type": "string", "description": "relative path, / separators"},
			"content": map[string]any{"type": "string", "description": "full file content"},
			"append":  map[string]any{"type": "boolean", "description": "append instead of overwrite"},
		},
		[]string{"path", "content"},
	),
	model.NewTool(
		"search_files",
		"Regex search over files inside the project root (Go regexp syntax, e.g. TODO|FIXME). "+
			"Results are file:line: text. Optionally restrict to files matching an include glob like *.go or *test*. Binary files and .agent/.git are skipped.",
		map[string]any{
			"pattern": map[string]any{"type": "string", "description": "regular expression"},
			"include": map[string]any{"type": "string", "description": "glob on file name, optional"},
		},
		[]string{"pattern"},
	),
}

var shellTool = model.NewTool(
	"run_command",
	"Run a shell command inside the project root. The command is shown to the user and requires explicit approval; it may be denied. "+
		"Runs non-interactively (no stdin), so commands that prompt for input will fail or hang until timeout. "+
		"Prefer file tools for inspection; use this for builds, tests, git and similar. "+
		"The command result includes stdout/stderr (possibly truncated) and the exit code.",
	map[string]any{
		"command":     map[string]any{"type": "string", "description": "shell command line"},
		"timeout_sec": map[string]any{"type": "integer", "description": "override the default timeout in seconds"},
	},
	[]string{"command"},
)

func AllTools() []model.Tool {
	tools := make([]model.Tool, 0, len(fileTools)+1)
	tools = append(tools, fileTools...)
	tools = append(tools, shellTool)
	return tools
}

func IsFileTool(name string) bool {
	for _, t := range fileTools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func decodeArgs(name, raw string, maxBytes int) (map[string]json.RawMessage, error) {
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("arguments too large")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("invalid JSON arguments: %v", err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

func getStr(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("field %q must be a string", key)
	}
	return v, nil
}

func getInt(m map[string]json.RawMessage, key string) (int, error) {
	raw, ok := m[key]
	if !ok {
		return 0, nil
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("field %q must be an integer", key)
	}
	return v, nil
}

func getBool(m map[string]json.RawMessage, key string) (bool, error) {
	raw, ok := m[key]
	if !ok {
		return false, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, fmt.Errorf("field %q must be a boolean", key)
	}
	return v, nil
}

func truncateArgs(raw string) string {
	const limit = 500
	if len(raw) > limit {
		return raw[:limit] + fmt.Sprintf("... (%d more chars)", len(raw)-limit)
	}
	return raw
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}
