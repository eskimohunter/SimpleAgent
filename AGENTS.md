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
    plan mode blocks `write_file` at dispatch and runs `run_command` only
    when allowlisted (no prompt); the engine advertises a reduced tool list
    and an ephemeral mode note per request. Enforcement order in
    `runCommandTool`: denylist -> mode policy -> prompt.
- Platform-paired files via build tags (`proc_unix.go`/`proc_windows.go`,
  `console_other.go`/`console_windows.go`), plus `runtime.GOOS == "windows"`
  branches in `paths.go`. Edits there must be verified with a Windows
  cross-build; Windows-only paths only get exercised by the Windows CI job.

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
- Symlink/junction tests `t.Skip` where symlinks are unavailable; real NTFS
  junction verification is manual (`docs/windows-test-checklist.md`).
- Session/audit state (`approvals.json`, `audit.jsonl`, `sessions/`) is
  created under `.agent/` inside the project root on first run; `bin/`,
  `dist/`, `.agent/`, and any `simpleagent.json` are gitignored.
- Releases: tagging `v*` (or `workflow_dispatch` with a `tag` input) runs
  `.github/workflows/release.yml`, which builds `make dist` and attaches
  assets to the GitHub release. Don't hand-roll release publishing.
