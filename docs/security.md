# Security model

SimpleAgent treats the LLM output as untrusted. This document states what is
enforced, how, and what residual risks remain by design.

## Threat model

- The model host is on your LAN and is reachable only at the configured
  `base_url`. The harness makes exactly one kind of network call: POST
  `{base_url}/chat/completions`. There is no other network code path
  (no telemetry, no auto-update, no other endpoints).
- The model may try to read sensitive files, write outside the project,
  or run arbitrary commands. All three are constrained:
  1. file tools → **code-enforced containment** (no prompt per op),
  2. shell commands → **human approval** (prompt per command),
  3. everything → **append-only audit log**.

## Enforcement layers

### 1. Path containment (file tools)

Every file op goes through `sandbox.Root.Resolve` (`internal/sandbox/paths.go`):

- Relative paths are joined to the project root and cleaned; `..` escapes are
  rejected with a strict parent-boundary check.
- On Windows the root prefix comparison is case-insensitive (`EqualFold`),
  matching the filesystem semantics; `/` and `\` both accepted.
- Windows-only rejections: `\\?\` / `\\.\` prefixes, UNC paths, alternate
  data streams (`:`), and reserved device names (`CON`, `NUL`, `COM1-9`,
  `LPT1-9`, ...).
- **Symlink/junction hardening:** every existing ancestor of the resolved path
  is checked with `Lstat` for reparse/symlink status; any symlink must resolve
  (via `EvalSymlinks`) to a location inside the root. Dangling symlinks are
  rejected outright — this closes the classic "write through a dangling
  symlink to a target that gets created elsewhere" escape.
- `.agent/` (harness state) is protected: no read or write through the file
  tools, and it is hidden from listings and skipped by search.
- **Harness config is protected too:** `Root.Protect` registers the loaded
  config file (`--config`) and `<root>/simpleagent.json` — the latter even
  when it does not exist yet, so the agent cannot *create* a config that a
  later launch would auto-load. Resolve refuses protected paths before any
  filesystem access, symlinks resolving onto them are rejected, flat
  listings tag them `(harness config, protected)`, and recursive listings
  and search skip them. The model therefore cannot rewrite its own
  allowlist/denylist/model endpoint or inspect the config through the file
  tools.

### 2. Shell command execution (`run_command`)

- Runs with the project directory as working directory.
- Non-interactive: stdin is closed; commands that prompt hang until the
  timeout and are then killed with the whole process tree
  (`taskkill /T /F` on Windows, process-group kill on Unix).
- Env hygiene: variables whose name contains (case-insensitive) `TOKEN`,
  `SECRET`, `PASSWORD`, `CREDENTIAL`, `APIKEY`, `API_KEY`, `AUTH`, `KEY`,
  `PASSWD` are stripped before the child starts, so the model config or any
  ambient secret cannot leak into a shell the model drives. `TEMP`/`TMP`
  point at `.agent/tmp`.
- Output is streamed to you live and captured as a bounded tail
  (default 256 KB per stream) with a truncation flag.
- Timeouts (default 120 s per command, 600 s max override).
- **Approval gate:** every command is shown verbatim. You choose
  `y` (run once), `a` (always — persisted to `.agent/approvals.json`), or
  `n` (deny; the model receives an explicit "DENIED" result and must adapt).
  A denied command is never executed. Matching against the config
  `approvals.allowlist` (exact or `prefix*`) auto-approves without prompting.
- **Denylist (hard block):** the config `approvals.denylist` (same exact /
  `prefix*` syntax, case-insensitive matching) is checked **first**, before
  the allowlist and before any prompt. A matching command never runs and is
  never auto-approved: deny always wins over the allowlist and over an
  "always" answer, because the denylist is the operator's policy rather than
  the model's (or an inattentive moment's) choice. Audited as
  `approval_denied` with `reason: denylist`. Matching is limited to the
  command's literal start (same prefix semantics as the allowlist), so a
  wrapper form such as `bash -c "curl …"` or `/usr/bin/curl …` is not
  matched and still goes through the approval prompt. The shipped example
  config
  denylists common network-transfer commands (`curl`, `wget`, PowerShell's
  `Invoke-WebRequest`/`Invoke-RestMethod`, `nc`, `ssh`, ...) so project
  files cannot be sent off the machine through an approved shell command;
  the built-in defaults ship an empty denylist — enable it deliberately.

### 3. Audit trail

`.agent/audit.jsonl` records every user message, assistant reply, tool call,
approval decision, and command result (exit code, timing, truncation), all
timestamped. Contents of files are not logged.

### 4. Conversation discipline

- Tool results sent to the model are truncated; read caps and listing caps
  apply at the tool level (see config `files` and `shell.max_output_bytes`).
- Context is bounded (message window + per-turn tool-call limit) so a
  runaway loop cannot spin forever.
- **Project instructions are advisory.** When `session.project_instructions`
  is enabled (default) the harness appends `<root>/AGENTS.md` (falling back
  to `<root>/CLAUDE.md`) to the system prompt; `session.system_prompt_file`
  replaces that with one explicit file. This text is untrusted input read
  at startup, exactly like the model's own output: it is *advice*, injected
  under a header that labels it project-controlled, and all enforcement
  (path containment, denylist, approval gate, audit) lives in code that
  project files cannot touch. Files are capped at 64 KiB, empty files
  inject nothing, discovered files skip symlinks, and an unreadable or
  oversized explicit file aborts startup rather than silently changing the
  prompt. The audit log records which instruction files were injected (name
  and size only — not their content, which is sent to the model endpoint
  as part of the system prompt, so instruction files must not contain
  secrets).

## Residual risks (by design)

1. **Approved commands run with your user rights.** An approved command can
   read/write anywhere you can, including reaching the network or your LAN.
   The audit log records it; nothing can make it *harmless*. Treat the
   approval prompt as a real security decision. If you need OS-level
   confinement of commands (no network, no filesystem outside the project),
   the upgrade path is running the child in a Windows **AppContainer**
   (low-integrity restricted token) — planned, not yet implemented.
2. **Toolchain side effects.** Compilers and package managers write caches
   (e.g. `%USERPROFILE%\.cache`, `go build` cache). Only `TEMP`/`TMP` are
   redirected today, so approved build/test commands may create files in your
   user profile. This is a side effect of an approved command, not a tool
   path, but be aware.
3. **Junction/symlink detection** relies on Go's reparse-point handling.
   Covered by `Lstat` + `EvalSymlinks` checks; still verify on your Windows
   build with the manual checklist.
4. **Model server trust.** The model endpoint sees the conversation and the
   contents of the files the agent reads. Only point it at a server you
   control.
5. **Prompt-level trust.** The agent is instructed to stay in the sandbox, but
   instructions are not enforcement: a hostile model could still *ask* you to
   approve a bad command. The approval prompt is the real control.

## Windows note

Process-level network blocking of child shells would require an admin-level
firewall rule or WFP driver — deliberately out of scope for a no-install,
portable tool. The "no internet" property holds for the harness itself and for
everything the agent can do without your explicit approval.
