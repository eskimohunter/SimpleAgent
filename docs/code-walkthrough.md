# SimpleAgent — Guided Code Walkthrough

This document is a reading guide to the source code, written for a developer
who is new to Go and to this codebase. It complements:

- `README.md` — how to build, configure and run SimpleAgent;
- `docs/security.md` — the threat model and what is enforced where;
- `plan.md` — the original design (architecture diagrams, tool table).

Every source file now carries package/type/function comments; read this guide
first for the map, then open the files in the order below and let the
comments explain the details. Line anchors refer to the current files.

---

## 1. The big picture

SimpleAgent is a CLI coding agent with a hard trust boundary. An LLM
(model) is given four file tools and one shell tool. Because the model's
output is untrusted text, the harness does not *ask* it nicely to behave — it
*enforces* the boundary in code:

| What the model can do | Gate | Enforced by |
|---|---|---|
| `list_files`, `read_file`, `write_file`, `search_files` | none per-op | `sandbox.Root.Resolve` — every path checked (`internal/sandbox/paths.go:186`) |
| `run_command` (shell) | deny → allowlist → approval prompt | `agent.Engine.approveCommand` + `approvals` |
| anything else | — | impossible: no other tools, no network code path |

Everything the agent does is written to an append-only audit trail
(`internal/audit`) and a per-conversation session log (`.agent/sessions/`).

Packages and their roles:

```
cmd/simpleagent     entry point: wires everything together
internal/config     merged configuration (defaults → file → env)
internal/model      OpenAI-compatible chat client + SSE parser + wire types
internal/agent      the agent loop: history, model calls, tool dispatch
internal/repl       interactive console: prompts, streaming, Ctrl+C
internal/approvals  command gates: allowlist + persisted "always" (auto-approve) and denylist (hard block)
internal/audit      append-only JSONL event log
internal/sandbox    the security core: path containment, file tools, exec
internal/mock       scripted offline model server for --mock
```

---

## 2. Reading order

### Step 1 — `cmd/simpleagent/main.go` (106 → ~150 lines)
The whole program in one file: flags, config, state dir, audit, approvals,
model client (or mock), engine, REPL. Start at `run()` (`main.go:53`) and
follow the numbered steps in the comments. This is where you learn **what
objects exist**; the rest of the tour shows what each one *does*.

### Step 2 — `internal/config/config.go`
Configuration is built in layers (`Load`, `config.go:228`):
built-in `Defaults()` → `simpleagent.json` → `AGENT_*` env vars → flags.
Pay attention to the security rules:
- `APIKey()` (`config.go:272`) reads the key from an environment variable —
  the config file only stores the variable's *name*;
- `checkAPIKey()` (`config.go:321`) rejects keys pasted into the config and
  allows keyless `https` only on loopback hosts;
- `Validate()` (`config.go:172`) runs last, so startup fails fast with a
  clear message instead of crashing mid-session.

### Step 3 — `internal/model/types.go` and `client.go`
`types.go` defines the **JSON shapes** of the chat API. The most
confusing detail is `Message.Content *string` (`types.go:23`) — a pointer so
the JSON field disappears when nil; the constructors (`TextMessage`,
`AssistantMessage`, `ToolResultMessage`) hide that. `Tool`/`NewTool`
(`types.go:89`) build the JSON-Schema tool catalog.

`client.go` is where the network happens — the *only* place in the program.
`Chat` (`client.go:60`) streams: the server answers with many small SSE
frames, and the code reassembles them. Read the comments around `onLine`
carefully: text fragments are concatenated, and tool calls are rebuilt
*fragment by fragment, indexed by chunk number* (`toolByIndex`). This is the
subtle part of the whole project.

### Step 4 — `internal/agent/tools.go` and `engine.go`
- `tools.go` declares what the model may call. The descriptions are read by
  the model, so they double as instructions; the enforcement happens in the
  sandbox regardless.
- `engine.go` is the loop. `RunTurn` (`engine.go:386`) is the most important
  function in the program: send history → model answers → dispatch its tool
  calls → feed results back → repeat until it answers in plain text. Read
  also:
  - `New` (`engine.go:116`) — assembles the system message at index 0 from
    the built-in rules (`buildSystemPrompt`) plus, when configured, the
    project's own instruction file: `projectInstructions` (`engine.go:164`)
    loads `session.system_prompt_file`, else discovers `<root>/AGENTS.md`
    (falling back to `CLAUDE.md`). Files are capped at 64 KiB and the
    content is advisory only — enforcement stays in code;
  - the plan/build `Mode` (`engine.go:42`, `Mode()`/`SetMode()`
    `engine.go:93`) — plan mode blocks `write_file` in `dispatchTool`
    (`engine.go:474`) and gates `run_command` in `runCommandTool`
    (`engine.go:595`); the request loop hides write_file/run_command from
    the advertised tools (`toolsForMode` in tools.go) and appends an
    ephemeral mode note;
  - `trimmedSlice` (`engine.go:328`) — history is capped for the model
    context, but never in a way that orphans tool results;
  - `runCommandTool` (`engine.go:595`) — the approval gate in action;
  - `dispatchTool`/`runFileTool` — argument decoding and routing.

### Step 5 — `internal/repl/repl.go` and `ui.go`
The human side. `repl.go:65` (`Run`) is the main loop; the Ctrl+C handler
above it is a neat trick (cancel the running turn first, quit on the second
press). `ui.go`'s `TextUI` implements the `agent.UI` interface
(`engine.go:34`) — an example of Go interfaces: the engine only knows the
interface; the REPL provides the concrete console. `ui.go` also shows a
mutex-guarded writer shared by several goroutines. When input is a real
terminal (`TextUI.Interactive`), the REPL enters raw character mode
(`enterRawMode` in the `console_*.go` files — termios on Linux,
`SetConsoleMode` on Windows) and reads keys itself: `ReadUserInteractive`
(`ui.go:277`) echoes printable keys, handles Backspace, and turns a Tab
press into the plan/build toggle (`REPL.toggleMode`, `repl.go:148`). It
also owns the command menu: typing `/` lists the built-in commands
(`slashCommands`, `repl.go:184`) below the input, ↑/↓ move the highlight,
Enter executes the selection (returned to the REPL as a normal slash
line), Tab accepts it into the line and Esc dismisses the menu for the
rest of the line; typing filters via `slashMenu` (`repl.go:197`).
`menuEntry.confirm` guards `/exit` and `/new` so a stray Enter on a
partial prefix submits the draft instead of quitting or resetting. Piped
input skips raw mode entirely; use `/mode` there.

### Step 6 — `internal/approvals` and `internal/audit`
Two small, self-contained packages. Approvals answers "may this command run
without asking?" — the allow side (config prefix rules + persisted "always"
commands, `Allowed`, `approvals.go:188`) auto-approves; the deny side
(`Denied`) hard-blocks matching commands regardless of allowlist or human
answer, and is checked first by the engine. Deny matching is
case-insensitive because the default Windows shell is. Note the
whitespace-normalizing `normalize` (`approvals.go:180`) and the atomic
tmp+rename save (`approvals.go:213`).
Audit appends one JSON line per event, flushed immediately so a crash cannot
lose the trail (`audit.go:42`).

### Step 7 — `internal/sandbox` — read this twice
This is the security core; `docs/security.md` explains the threat model.

- `paths.go` — **the gate.** `Root.Resolve` (`paths.go:186`) turns any
  model-supplied path into an absolute path inside the project root, and
  rejects everything else: NUL bytes, Windows device names (`NUL`, `CON`…),
  alternate data streams (`:`), UNC/`\\?\` prefixes, `..` escapes, `.agent`,
  registered protected files such as the harness config (`Root.Protect`),
  and — the subtle part — symlinks/junctions via `checkSymlinks`
  (`paths.go:270`), which walks every existing ancestor and refuses links
  that escape or dangle or land on protected ground. Compare with
  `fs_test.go` and `paths_test.go`, which attack this code with real
  symlink escapes.
- `fs.go` — the file tools: listing (flat/recursive, entry- and
  depth-capped), reading (whole vs. paged with line numbers), writing
  (content-cap checked before touching disk), and search (a contained grep
  with many defensive caps — see the `Search` comment, `fs.go:436`).
- `exec.go` — the shell executor. Read the `tailWriter` (`exec.go:55`): a
  ring buffer keeping only the last N bytes of output. Then `Run`
  (`exec.go:177`): process spawn, **why stdout/stderr must be drained by
  goroutines** (a full pipe blocks the child forever), and the
  timeout/cancel race resolved with `sync.Once`.
- `proc_windows.go` / `proc_unix.go` — the platform halves of the same
  functions, chosen by build tags: process groups (`Setpgid` /
  `CREATE_NEW_PROCESS_GROUP`) so the child can never outlive the kill.

### Step 8 — `internal/mock/server.go`
A scripted OpenAI-compatible server so `--mock` works offline. `handleChat`
(`server.go:77`) shows the SSE *writer* side (the mirror of
`model/client.go`); `DefaultBrain` (`server.go:161`) is a tiny state machine
that deliberately triggers a `run_command`, so the demo exercises the
approval prompt too.

---

## 3. Anatomy of one turn (data flow)

```
you type a message
   │
   ▼
REPL.Run ──► Engine.RunTurn (engine.go:386)        [repl.go:65]
   │             │ appends your message to history + audit
   ▼             ▼
        Client.Chat (client.go:60)  ── POST {base_url}/chat/completions
        streaming SSE reply reassembled; text streamed to your screen
   │             │
   ▼             ▼
   ┌── model asked for tool calls? ── no ──► turn done (answer printed)
   │  yes
   ▼
dispatchTool (engine.go:474)
   ├── file tool? ─► Sandbox.ReadFile/WriteFile/List/Search
   │                  every path through Root.Resolve → contained, no prompt
   └── run_command? ─► approveCommand (engine.go:726)
                        deny rule match?  ─yes─► blocked: never runs,
                        │                  no prompt (hard block)
                        │ no
                        ▼
                        allowlist match?  ─yes─► run
                        │ no
                        ▼
                        prompt you: [y]es once / [a]lways / [n]o
                        │ approved
                        ▼
                        sandbox.Run (exec.go:177): powershell/sh child,
                        env filtered, output capped, timeout kill
   │
   ▼
tool result appended to history, sent back to the model ──► loop again
   │
   └── (until: plain-text answer | Ctrl+C | tool-call limit)
 ```

The chain above is the build-mode flow. In plan mode the same dispatch
refuses `write_file` outright and short-circuits `run_command` to "allowlist
match only" — no prompt — so the diagram's deny → allowlist → prompt ladder
applies to plan mode with the prompt step removed for non-allowlisted
commands.

Every hop along the way is also mirrored into `.agent/audit.jsonl`
(events) and the session JSONL (messages), so any single action can be
reconstructed later.

---

## 4. Go concepts cheat-sheet (with "first appears in")

New to Go? The code uses a handful of idioms over and over. Each entry says
what the concept is and where to see it first.

| Concept | One-line meaning | First real example |
|---|---|---|
| `internal/` packages | `internal` cannot be imported from outside this module — a language-level way to keep these packages private | every `import "simpleagent/internal/..."` in `main.go` |
| Errors as values | No exceptions: functions return an `error`; callers check it. `fmt.Errorf("…: %w", err)` wraps an error to add context | `config.go:172`, `main.go:53` pattern |
| `defer` | "Run this when the function returns" — cleanup written next to the resource | `auditLog.Close()` in `main.go`; `defer f.Close()` in `fs.go:317` (`readRanged`) |
| Pointers `*T` | A pointer stores a memory address; methods taking a pointer receiver (`func (e *Engine)`) can mutate the struct | `engine.go:386` `RunTurn` |
| `&x` and why `Content *string` | The chat API omits absent fields; `nil` pointer → field omitted (`types.go:23`); `TextMessage` copies its param to take its address (`types.go:47`) |
| Interfaces | A set of method signatures; any type implementing them satisfies the interface. `agent.UI` is implemented by `repl.TextUI` | `engine.go:34` + `ui.go:29` |
| `io.Reader`/`io.Writer` | Interfaces for "source of bytes"/"sink of bytes" — `os.Stdout`, files, pipes all satisfy them | `NewTextUI(in io.Reader, out io.Writer)` `ui.go:41` |
| Goroutines `go f()` | Run `f` concurrently on another thread | reader goroutines in `exec.go:177` |
| Channels + `select` | Channels pass values between goroutines; `select` waits on several at once | ctx watcher in `exec.go:177`; approval prompt ctx in `ui.go:177` |
| `context.Context` | Carries cancellation (Ctrl+C, timeouts) through calls | `RunTurn(ctx, …)` `engine.go:386` |
| `sync.Mutex` | Serialize access to shared data from several goroutines | `TextUI.mu` (`ui.go:29`), `tailWriter` (`exec.go:55`) |
| `sync.Once` | "Run this exactly once, even if called concurrently" — the kill race | `exec.go:177` |
| `sync.WaitGroup` | Wait until N goroutines finish | `exec.go:177` |
| JSON struct tags | `` `json:"name"` `` maps a field to a JSON key; `omitempty` skips empty values | every struct in `types.go` |
| `json.RawMessage` | Keep raw JSON bytes undecoded until needed | `decodeArgs` `tools.go:120` |
| `bufio.Scanner` | Read a stream line by line | SSE parsing `client.go:60` |
| Build tags | `//go:build windows` keeps a file in the build only on Windows | `proc_windows.go:1`, `console_other.go:1` |
| Closures | Functions defined inline that capture surrounding variables (also used for recursive walking via `var walk func…`) | `listRecursive` `fs.go:153`, `onLine` `client.go:60` |
| `strings.Builder` | Efficiently assemble a big string piece by piece | everywhere in `fs.go` listings |
| Sentinel errors | Predefined errors compared with `errors.Is`, so behavior ≠ "string match" | `paths.go:24`, `errStopListing` `fs.go:225` |
| `map[string]struct{}` | A set: only the keys matter, values cost nothing | `approvals.go:50` (ruleSet `exact` map) |

---

## 5. Project-specific terms

| Term | Meaning |
|---|---|
| tool / tool call | A function the model can request, described by JSON Schema (`tools.go:31`); "tool call" is one request with arguments |
| SSE | Server-Sent Events: the HTTP streaming format (`data: …` lines) the model server uses (`client.go:60`) |
| approval gate | The prompt before every shell command; "always" adds it to the persisted allowlist |
| allowlist | Config entries (exact or `prefix*`) that auto-approve commands |
| denylist | Config entries (exact or `prefix*`, matched case-insensitively) that HARD-BLOCK commands — checked before the allowlist and the prompt, never overridable at runtime (`engine.go` `approveCommand`; example entries in `simpleagent.json.example`) |
| session | One conversation: an in-memory history + one JSONL file in `.agent/sessions/` |
| audit | `.agent/audit.jsonl`: one JSON line per *event*, written for accountability |
| sandbox | The confinement layer: `Root.Resolve` (paths) + capped file tools + safe exec |
| `.agent` | The harness state directory inside the project root — invisible to and protected from the model |
| protected file | Harness-owned file registered via `Root.Protect` (the loaded config and `<root>/simpleagent.json`): `Resolve` refuses it — even for creation — symlinks onto it are rejected, listings tag it `(harness config, protected)` and search/recursive listings skip it |
| tail | The last N bytes of command output kept for the model (`exec.go:55`) |
| truncation | What happens when output exceeds a cap — flagged, never silently dropped without a marker |

Suggested next step: run `--mock` (see README) and watch the log files in
`.agent/` grow while the demo runs — reading the audit trail live is the
fastest way to connect this map to behavior.
