# SimpleAgent

A single-file, no-install, offline-first CLI coding agent for Windows 11.
Go + stdlib only (zero external dependencies). The harness confines the agent
to the project directory, runs shell commands only with human approval, keeps
an audit trail, and never touches the internet except for the model endpoint
you configure on your LAN.

```
user> find the failing test in this project and fix it
  ...the agent lists/reads/search files (confined to the project dir)...
$ go test ./...
approve command? [y]es once / [a]lways / [n]o
```

## Quick start

Build (any OS with Go 1.22+):

```
make build-windows     # -> bin/simpleagent.exe   (Windows 11, amd64)
make build             # -> bin/simpleagent       (native)
```

Run on Windows 11 — no install needed:

```
simpleagent.exe --root C:\path\to\your\project
```

### Set the API key and run against your model

The harness reads the key from an environment variable (named by
`api_key_env` in the config, `AGENT_API_KEY` by default) — the key itself
never goes into `simpleagent.json`.

Windows 11 (PowerShell):

```powershell
$env:AGENT_API_KEY = "sk-or-v1-..."
.\simpleagent.exe --root C:\path\to\your\project
```

Windows 11 (Command Prompt):

```bat
set AGENT_API_KEY=sk-or-v1-...
simpleagent.exe --root C:\path\to\your\project
```

Linux:

```sh
export AGENT_API_KEY="sk-or-v1-..."
./simpleagent --root /path/to/project
```

The variable lasts for the current terminal session only; to make it
persistent on Windows use `setx AGENT_API_KEY sk-or-v1-...` (takes effect in
new terminals). If your endpoint needs no key (e.g. an unauthenticated LAN
server), set `"api_key_env": ""` in the config.

First run creates the harness state directory inside your project:

```
.agent/
├── approvals.json     # commands you approved with "always"
├── audit.jsonl        # append-only log of every tool call and decision
├── sessions/          # conversation history (JSONL)
└── tmp/               # TEMP/TMP for shell commands
```

### No model server yet? Try the offline demo

```
simpleagent.exe --mock --root C:\path\to\your\project
```

(same on Linux: `./simpleagent --mock --root /path/to/project`) —
`--mock` runs a built-in scripted model server on 127.0.0.1, useful to verify
the harness works before you point it at a real model. No API key needed.

## Configuration

Config file: `simpleagent.json` in the project root (or pass `--config`).
Start from `simpleagent.json.example` in this repository, copy it to the
project root and edit. Shown values match the built-in defaults except:
`model.base_url` and `model.model` are placeholders and are required unless
you run with `--mock`; the `approvals.allowlist` entries are recommendations,
not defaults — commands matching them run **without** an approval prompt, so
curate that list. The shell (`powershell.exe` on Windows, `sh` elsewhere)
uses built-in per-OS defaults when `command`/`args` are omitted.

```json
{
  "root": ".",
  "model": {
    "base_url": "http://192.168.1.50:8000/v1",
    "model": "your-model-name",
    "api_key_env": "AGENT_API_KEY",
    "insecure_skip_verify": false,
    "temperature": 0.2,
    "timeout_sec": 300
  },
  "shell": {
    "default_timeout_sec": 120,
    "max_output_bytes": 262144
  },
  "files": {
    "max_read_bytes": 1048576,
    "max_write_bytes": 5242880
  },
  "approvals": {
    "allowlist": ["git status", "git diff*"],
    "persist": true
  },
  "session": {
    "max_messages": 200,
    "max_tool_calls_per_turn": 25
  }
}
```

Environment overrides: `AGENT_BASE_URL`, `AGENT_MODEL`, `AGENT_TEMPERATURE`.
The API key is read from the env var named by `api_key_env` — that field must
be the variable's *name*, never the key itself, and the key never goes in the
config file. The harness refuses to start if `api_key_env` holds something
that cannot be an environment variable name, or if the named variable is empty
when talking to a remote `https` endpoint (set `api_key_env` to `""` if that
endpoint genuinely needs no key). Do not commit `simpleagent.json` if it
contains LAN details.

The model server must expose the OpenAI-compatible `/v1/chat/completions`
endpoint **with tool calling support** (vLLM, Ollama, LM Studio, llama.cpp
server etc.).

## Commands

| key / command | effect |
|---|---|
| plain text     | send a message to the agent (multiline: end a line with `\`) |
| `y` / `a` / `n` | at an approval prompt: run once / always allow / deny |
| `/approvals`   | show the current allowlist and persisted approvals |
| `/new`         | reset conversation (new session file) |
| `/help`        | this list |
| `/exit`        | quit |
| `Ctrl+C`       | interrupt the running turn (again at idle: quit) |

## Agent capabilities

The agent can only:

- `list_files`, `read_file` (line numbered, paged), `write_file`, `search_files`
  — all machine-enforced inside the project root. `.agent/` is off-limits.
- `run_command` (PowerShell on Windows) — always shown to you first and
  approval-gated, unless the command matches the configured allowlist or a
  previously "always"-approved command.

It has no network tool and no way to touch anything outside the project root
except through shell commands you explicitly approve.

## Security

See `docs/security.md` for the threat model, what is enforced where, and the
honest residual risks. `docs/windows-test-checklist.md` is the manual
verification checklist to run on a Windows 11 machine after building.
