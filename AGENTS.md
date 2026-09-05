# AGENTS.md

SimpleAgent: a sandboxed CLI coding agent for Windows 11, written in Go.
Zero external dependencies (`go.mod` has no requires, no `go.sum`, and
`CGO_ENABLED=0` must keep working) — adding a dependency is a decision, not
a default. The product targets Windows 11 but is developed and tested on
Linux; CI (`.github/workflows/ci.yml`) runs Linux and Windows jobs.

## Reading order / architecture

- `cmd/simpleagent/main.go` is the wiring diagram: flags → `config.Load` →
  `sandbox.NewRoot` → `.agent` state dir → audit → approvals → model client
  (or mock) → engine + REPL.
- Guided learning guide with file:line anchors: `docs/code-walkthrough.md`.
  **Anchors go stale when code moves — update them when you edit the files
  they cite.**
- One-page architecture map: `docs/architecture.png`, hand-authored in
  `docs/architecture.svg` — edit the SVG and re-render (rsvg-convert at 2x)
  when the wiring changes, then update the walkthrough's anchor for it.
- Threat model / enforcement layers: `docs/security.md`.
- Trust model: LLM output is untrusted; boundaries are enforced in code.
  The security-critical gate is `Root.Resolve` (`internal/sandbox/paths.go`):
  every file tool path passes through it. Preserve its semantics:
  - `.agent/` is always blocked; the harness config (`--config` path and
    `<root>/simpleagent.json`, even if absent) is registered via
    `Root.Protect` and blocked the same way; symlink/junction escapes and
    dangling symlinks are rejected.
  - Approval rules (`internal/approvals`): allowlist = case-sensitive
    exact/`prefix*`, whitespace-normalized, auto-approves; denylist =
    case-insensitive, hard-blocks, checked first and wins over allowlist
    and human "always". `approvals.New` takes `(storeFile, persist,
    allowlist, denylist)`.
  - Plan/build modes (Tab at the prompt, `/mode`, or `Engine.SetMode`):
    sessions start in **plan mode**; plan mode blocks `write_file` at
    dispatch and runs `run_command` only when allowlisted (no prompt); the
    engine advertises a reduced tool list and an ephemeral mode note per
    request. Enforcement order in `runCommandTool`: denylist -> mode
    policy -> prompt.
- Platform-paired files via build tags: `internal/sandbox/proc_unix.go` /
  `proc_windows.go` and, for console input, THREE repl files —
  `console_unix.go` (linux termios raw mode), `console_windows.go`
  (`SetConsoleMode`), `console_other.go` (`!windows && !linux` cooked
  fallback, darwin included). Plus `runtime.GOOS == "windows"` branches in
  `paths.go`. Edits there must be verified with Windows (and darwin) cross
  builds; Windows-only paths only get exercised by the Windows CI job.
- REPL input is line-based when piped; on a real TTY the raw-mode reader in
  `internal/repl/ui.go` (readRawLine) drives echo/Backspace, the Tab
  plan/build toggle, and the `/`-command menu (arrows select, Enter runs,
  Tab accepts, Esc dismisses for the line). Commands live in one table,
  `slashCommands` in `repl.go`: it feeds both `/help` and the menu, and
  `/exit` + `/new` + `/update` are confirm-guarded there. Raw-mode behavior
  is never exercised by piped CI — it needs the staged-input unit tests
  (`internal/repl/ui_test.go`) and a manual terminal check. Prompt reads
  (`readSingleLine` for pipes, `readPrompt` for TTYs) accept bare `\r`,
  bare `\n`, or CRLF as the line end and never depend on the tty's ICRNL
  (CR→LF translation) setting: cooked reads hang forever when ICRNL is
  off, so interactive approval/confirm prompts (`readPrompt`) read in raw
  mode where every key, including Enter, arrives as a plain byte.
- `/update` (`internal/update`): the second network code path (after the
  model client), user-triggered only. Hardcoded repo
  `eskimohunter/SimpleAgent`. The newest release is compared against the
  Makefile-stamped version (`internal/update/version.go`, numeric
  `vX.Y.Z`): `releases/latest` when the repo has a stable release,
  otherwise the highest-versioned release from the full `/releases` list
  (prereleases included, drafts skipped — a prerelease-only repo must not
  make /update report "nothing to update to"). Only the platform binary
  asset is accepted and only with the
  release's `SHA256SUMS`; install is download → verify SHA-256 → stage
  (next to the binary, same filesystem, chmod'd to the original's mode so
  the rename cannot strip the execute bit) → swap (`swap_unix.go` atomic
  rename / `swap_windows.go` `exe → exe.old` dance with rollback) →
  restart: Unix re-execs in place (`syscall.Exec`, `repl/restart_unix.go`
  — same PID/terminal, or the shell's job control would stop the child
  with SIGTTIN); Windows spawns with inherited stdio
  (`repl/restart_windows.go`). Verified in
  `internal/update/update_test.go` against an httptest fake GitHub.
  An offline or no-release run reports and continues; the success path
  (swap + restart) is covered by unit tests, not by real GitHub.

## Commands

```sh
make build             # bin/simpleagent  (native, CGO_ENABLED=0, -trimpath)
make build-windows     # bin/simpleagent.exe
make dist              # release tarballs + SHA256SUMS into dist/
make test              # CGO_ENABLED=0 go test ./...
make vet               # CGO_ENABLED=0 go vet ./...
```

Full local verification (mirrors CI, run before finishing any change):

```sh
gofmt -l .                                   # CI checks git-tracked *.go only
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -race -count=1 ./...
CGO_ENABLED=0 go test ./...                  # Linux
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

Focused runs: `CGO_ENABLED=0 go test ./internal/sandbox -run 'TestProtected|TestSymlink' -v`.
`go` is not on PATH in this workspace's shell on some machines — the Go 1.26
toolchain used for builds lives in the Nix store (see shell history) when
`make`/`go` are missing.

Smoke test the built binary offline (what CI does):

```sh
d=$(mktemp -d)
printf 'hello agent world\na\n/exit\n' | ./bin/simpleagent --mock --root "$d"
```

`--mock` runs the built-in scripted server (`internal/mock`), which is also
how engine tests drive the agent. Real runs need a config: model via
`base_url`, API key read from the env var *named* by `api_key_env`
(`AGENT_API_KEY` by default), plus `AGENT_BASE_URL`/`AGENT_MODEL`/
`AGENT_TEMPERATURE` env overrides.

## Conventions and gotchas

- Config layering: defaults live in `config.Defaults()`; `simpleagent.json`
  overlays; `--config` and `--root` flags win. `simpleagent.json.example`
  and the README config block show **recommendations, not defaults**
  (e.g. the denylist is empty by default). When you change config surface,
  keep `Defaults()`, the example file, the README block, `docs/security.md`,
  and the walkthrough in sync — commits in this repo bundle those updates.
- Version stamping: Makefile injects `main.version` via `-X` from
  `git describe`; the `version` var in `cmd/simpleagent/main.go` is only a
  fallback. Build with the Makefile flags (or `go build ./...` for a dev
  binary) so `--version` is meaningful.
- Engine tests (`internal/agent/engine_test.go`) that run real commands use
  `sh`-compatible commands (Linux); Windows command behavior is covered by
  CI smoke tests, not unit tests.
- `agent.New` returns `(*Engine, error)` since it assembles the system
  prompt and may load project instructions: `session.project_instructions`
  (default true) appends `<root>/AGENTS.md` (else `CLAUDE.md`) to the
  prompt, and `session.system_prompt_file` overrides it — a missing,
  oversized or symlinked explicit file aborts startup. The content is
  advisory only. Engine tests use the `mustNew`/`mustBuild` helpers, which
  fail on startup errors; note the repo's own AGENTS.md is loaded whenever
  SimpleAgent runs on this repository.
- Symlink/junction tests `t.Skip` where symlinks are unavailable; real NTFS
  junction verification is manual (`docs/windows-test-checklist.md`).
- Session/audit state (`approvals.json`, `audit.jsonl`, `sessions/`) is
  created under `.agent/` inside the project root on first run; `bin/`,
  `dist/`, `.agent/`, and any `simpleagent.json` are gitignored.
- Releases: tagging `v*` (or `workflow_dispatch` with a `tag` input) runs
  `.github/workflows/release.yml`, which builds `make dist` and attaches
  assets to the GitHub release. Don't hand-roll release publishing.
