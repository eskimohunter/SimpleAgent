# Windows 11 manual test checklist

Run after building `bin/simpleagent.exe` (`make build-windows` on any machine
with Go). Use a scratch project directory, e.g. `C:\tmp\agent-test`, and keep
a second terminal handy.

## 1. Basic run + mock demo

```
simpleagent.exe --mock --root C:\tmp\agent-test
```

Expected:
- banner shows project root and "mock (built-in demo server)"
- a `.agent` directory appears inside the project root
- the demo agent lists files, then asks to run a command

Answer `y` → command output streams live, agent finishes. `/exit` quits.

## 2. UTF-8 / console

Inside the sandbox create a file with non-ASCII content (e.g. via
`write_file` through a demo run, or notepad), then have the agent `read_file`
it. Characters must render correctly (umlauts, CJK). Check the console prompt
glyphs (`»`, `$`) are not mojibake — output codepage is forced to 65001.

## 3. Approvals

- Answer `n` to a command → agent reports the denial and proposes an
  alternative; the command never runs.
- Answer `a` to `echo persisted-ok` → `.agent\approvals.json` contains the
  command. Exit and restart the harness, ask the mock/demo (or a real model)
  the same command again → no prompt (auto-approved).
- `/approvals` lists allowlist + persisted commands.
- Press `Ctrl+C` while the agent is streaming a reply → turn is interrupted,
  harness does not exit. `Ctrl+C` again at the prompt → harness exits.
  (If stuck in an approval prompt, press Enter once after Ctrl+C.)

## 4. Path containment (file tools)

From the project root open a second console and create a junction:

```
mklink /J C:\tmp\agent-test\escape C:\Windows
```

Now, with a real model or the demo variant that reads files, ask the agent to
`read_file escape\win.ini` (or write `escape\pwn.txt`). Expected: the tool
returns an ERROR ("escapes project root" / junction rejection) and nothing
outside the sandbox is read or created. Then `rmdir escape`.

Also verify:
- `read_file ..\somefile` and `C:\Users\...` absolute paths → ERROR
- `.agent\audit.jsonl` read attempt via file tool → ERROR (protected)
- files with spaces/unicode names work normally

## 5. Shell behavior

- Ask to run `Start-Sleep -Seconds 60` with `timeout_sec: 1` → command is
  killed promptly (well under 60 s), result says "timed out and was killed".
- Ask to run a command that writes a lot (`1..100000 | %{ $_ }`) → result is
  truncated with a flag, only the tail is shown, console did not freeze.
- Run `git status` in a non-git dir → exit code 128 shown; agent recovers.
- Confirm env hygiene: in a config, set `AGENT_API_KEY=supersecret` (and
  `FOO_SECRET=x`), approve `$env:AGENT_API_KEY; $env:FOO_SECRET` → both print
  empty.

## 6. Audit trail

After a session, open `.agent\audit.jsonl` — every message/tool call/approval/
command result with timestamps is present. File contents are not logged.

## 7. Restart persistence

Restart the harness in the same root. Banner shows the same root. The
`approvals.json` contents survive (see 3). `/new` starts a fresh session file
under `.agent\sessions`.

## 8. No network for tools (approval-gated)

Approved commands may reach the network by design — this is documented. To
verify the harness itself has no egress other than the model endpoint, run
with `--mock` (server on 127.0.0.1) and inspect with a firewall log or
`netstat` while the agent works: no connections besides localhost appear.
