# SimpleAgent — Implementation Plan

A single-file, no-install, offline-first CLI coding agent for Windows 11. Go + stdlib only (zero external deps), cross-compiled on this Linux box into `simpleagent.exe`, developed and unit-tested here, manually verified by the user on Win11.

## 1. Architecture overview

```
┌─────────────────────────────── Windows 11 console ───────────────────────────────┐
│  REPL (user chat, approval prompts, /commands)                                   │
└───────────────┬───────────────────────────────────────────────▲──────────────────┘
                │ messages / tool calls                         │ stream + tool results
┌───────────────▼───────────────────────────────────────────────┴──────────────────┐
│  Agent loop: history mgmt, truncation, iteration cap, tool dispatch              │
├───────────────┬──────────────────────────────┬────────────────────────────────────┤
│  Model client│ File tools (sandbox gate)    │ run_command (approval gate)         │
│  SSE stream   │ read/write/list/search      │ PowerShell child proc               │
│  tool calling │ ONLY inside project root    │ +env hygiene +timeout+tree kill     │
└───────────────┴──────────────────────────────┴────────────────────────────────────┘
        │                    │                          │
        ▼                    ▼                          ▼
   LAN model host    project root files         .agent/ state (audit, approvals,
  (OpenAI-compat)    (the only dir tools touch)  sessions, tmp, logs)
```

**Trust boundary:** LLM output is untrusted. File tools are machine-enforced contained (never prompt per-op). `run_command` is human-approved (one-shot / always / deny). Everything the agent does is written to an append-only audit log.

## 2. Repository layout

```
SimpleAgent/
├── go.mod                        (module simpleagent; go 1.22+, no dependencies)
├── Makefile                      (build, test, build-windows)
├── README.md                     setup, config reference, security notes
├── docs/
│   ├── security.md               threat model + residual risks
│   └── windows-test-checklist.md manual Win11 verification checklist
├── internal/
│   ├── config/       config.go   load/merge/validate config (JSON)
│   ├── model/        client.go   OpenAI-compatible client, SSE parser (stream=true)
│   │                schema.go    tool schemas generated from Go structs
│   │                tools.go     tool-call result assembly/truncation
│   ├── mock/         server.go   scripted local OpenAI-compat server (--mock, offline demo)
│   ├── sandbox/
│   │   ├── paths.go              ★ path containment (core security)
│   │   ├── fs.go                 read/write/list/search with caps
│   │   └── exec.go               PowerShell child proc, env filter, timeout kill
│   ├── approvals/    approvals.go  session + persisted allowlists
│   ├── audit/        audit.go    JSONL audit writer
│   └── repl/         repl.go     REPL, /commands, approval prompts, Ctrl+C
└── cmd/simpleagent/main.go       wiring, flags, startup banner
```

## 3. Path containment — `sandbox/paths.go` (security core)

All file ops route through one gate `toSafePath(requested) → (absPath, error)`:
- Reject: `..` escapes after clean, drive-relative (`C:x`), UNC/`\\?\`/`\\.\`, Windows device names (`CON`, `NUL`, `COM1–9`, `LPT1–9`) anywhere in the path.
- Joins relative paths to root; **strict parent-boundary check** via `filepath.Rel` + case-insensitive compare (`EqualFold` — Windows FS is case-insensitive), accepting both `/` and `\` separators.
- **Symlink/junction hardening:** walk existing ancestors root→leaf, `Lstat` for reparse points, `EvalSymlinks` the resolved segment, verify target stays inside root. (Junctions are the classic Windows escape — treated like symlinks.)
- `.agent/` is blocklisted for the model entirely (harness-owned).

Cross-platform unit tests on Linux (incl. real symlink escape), plus a manual checklist item for NTFS junctions on Win11.

## 4. Tools exposed to the model

| Tool | Notes |
|---|---|
| `list_files(path, recursive)` | size-capped listing |
| `read_file(path, offset?, limit?)` | 1-based line numbers, read cap 1 MB (config) |
| `write_file(path, content, append?)` | under root only, 5 MB cap, creates parents |
| `search_files(pattern, include_glob)` | contained grep so benign searches never need approval |
| `run_command(command, timeout_sec?)` | **approval-gated** (below) |

No network, no delete, no env tools in v1.

## 5. `run_command` + approvals

- Spawn: `powershell -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command <cmd>`, cwd = project root, hidden window, `CREATE_NEW_PROCESS_GROUP`.
- **Env hygiene:** strip vars matching `TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL` (model config never exported); set `TEMP/TMP` → `.agent/tmp`.
- stdout/stderr tee'd live to the user (live visibility) **and** into a 256 KB ring buffer; on timeout kill the whole tree via built-in `taskkill /PID <pid> /T /F`.
- stdin closed (non-interactive; documented). Output returned to model = tail ≤ 200 KB + exit code + truncation flag.

**Approval prompt** renders the exact command; user picks: `Y` run once, `A` always (persisted), `N` deny → model gets a `denied by user` tool result and can recover. Denied calls are never returned as executed.

**Persistence:** approvals live in `.agent/approvals.json`, merged with a hand-curated allowlist in `simpleagent.json` (exact-match first, then curated prefix patterns). Survives restarts.

## 6. Agent loop

- History: in-memory + appended to `.agent/sessions/<timestamp>.jsonl` (resume support planned in a later milestone; v1 auto-persists + `/new` resets).
- Each turn: stream assistant text live; on a `tool_calls` frame, execute calls **sequentially** (file tools auto + audited, `run_command` gated), push `role:"tool"` results, continue loop until the model replies without tool calls.
- Guards: ≤ 25 tool iterations/turn; sliding window keeps system + first user message + last ~40 messages; big results truncated with explicit notes; request timeout 5 min; visible error → abort turn.

## 7. Model client

- `POST {base_url}/chat/completions`, `stream:true`, minimal hand-rolled SSE parser (stdlib only). Endpoint is **only** the configured LAN base URL — no other network use anywhere in the program.
- Config: `base_url`, `model`, optional `Authorization` bearer read from env var (not config file), `insecure_skip_verify` for self-signed LAN TLS, `temperature`.
- `--mock` flag starts the embedded scripted server on 127.0.0.1 → full E2E offline demo/CI without a model.

## 8. Config & state (inside project root only)

`simpleagent.json` in root (or `--config`, env, flags override):

```json
{
  "model": { "base_url": "http://192.168.1.50:8000/v1", "model": "…",
             "api_key_env": "AGENT_API_KEY", "insecure_skip_verify": false, "timeout_sec": 300 },
  "shell": { "default_timeout_sec": 120, "max_output_bytes": 262144 },
  "files": { "max_read_bytes": 1048576, "max_write_bytes": 5242880 },
  "approvals": { "allowlist": ["git status", "git diff*"], "persist": true },
  "session": { "max_messages": 200, "max_tool_calls_per_turn": 25 }
}
```

State dir `.agent/` (auto-created, gitignored): approvals, audit JSONL (every tool call + decision + timestamp), session logs, temp. README advises ignoring `simpleagent.json` if it holds LAN details.

## 9. CLI / UX

- `simpleagent.exe [--root dir] [--mock] [--config file]`; root defaults to cwd; banner shows root, endpoint (masked), session id, version.
- Prompt `user> `, streamed replies; `/help /new /approvals /exit`; Ctrl+C cancels streaming request, second Ctrl+C exits; force `chcp 65001`-equivalent (`SetConsoleOutputCP`) at startup for UTF-8.

## 10. Security posture (honest limits)

- **File tools:** code-enforced confinement to project root — no outside read/write possible, junction-aware.
- **Shell:** human approval is the control; an approved command runs with the user's rights and could in principle reach the LAN/network. Mitigated by visible command text, env hygiene, timeouts, tree kill, audit trail, temp redirection. AppContainer OS-sandboxing documented in `docs/security.md` as the upgrade path if that residual risk ever matters.
- **Harness:** no auto-update, no telemetry, no internet path except the configured model host. The model host must be on the LAN with an OpenAI-compatible endpoint supporting tool calling.

## 11. Milestones

1. **Scaffold** — go.mod, main, config loader, state-dir init, audit writer, Makefile
2. **Sandbox core** — `paths.go` + `fs.go` + table/symlink unit tests
3. **Model client + mock** — SSE streaming, tool-call assembly; REPL echoes text (no tools yet)
4. **Agent loop** — tool dispatch, history, truncation, integration test against mock
5. **Approvals + run_command** — PowerShell exec, approval UX, persistence, timeout kills
6. **Windows polish** — console UTF-8, Ctrl+C, cross-compile target, docs + manual Win11 checklist
7. *(stretch)* `/compact` summary, `edit_file`, binary sniffing

## 12. Known trade-offs / notes

- Toolchain side effects (e.g. Go/node caches) from *approved* commands will still write to the user profile unless env is redirected — v1 redirects `TEMP/TMP` only and documents the rest.
- The repo is currently empty; the Go module is initialized fresh.
