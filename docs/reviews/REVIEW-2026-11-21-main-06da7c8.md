# Third review: agent containment (`main` at `06da7c8`, installed, 2026-11-21)

**Verdict: the four one-line bypasses are closed and the install is sound on this PC. Two routes
remain: the containment repository can still receive `.claude\settings.json` and `.mcp.json` through
git, and the new Codex rules let Codex run and write almost anything inside a project.**

Scope: every change since `41dada9` (the Go, the install scripts, the Codex rules, the response
`RESPONSE-2026-11-21-main-41dada9.md`), and the installed copy on this PC, which reports commit
`06da7c8`.

How things were tested:

- Hook findings: each command was run through `hook.Decide` in a scratch copy of `06da7c8` with the
  suite's `testConfig`.
- Installed state: `agent-hook check` and `lab-broker check` were run from
  `C:\Program Files\agent-containment`, and access lists were read with `icacls`, all unelevated from
  the owner's account.
- Not tested: anything needing elevation, the lab VM itself, the scheduled tasks (not visible
  unelevated), and Codex's own behaviour.

## High

### 1. The containment repository can still be given `.claude\settings.json` and `.mcp.json` (tested)

All four of these pass in the containment project:

```
git fetch C:/work/wac/lab-out/<time>/r.bundle main
git checkout FETCH_HEAD -- .claude/settings.json .mcp.json
git pull https://github.com/someone/else.git main
git reset --hard FETCH_HEAD
```

The response says the agent has no route to a commit that holds such a file. It has two:

- **The lab.** Anything runs there, so a commit can be made in the guest, bundled, pulled into
  `lab-out`, and fetched from that path.
- **Any URL.** `fetch` and `pull` take any repository, not only `origin`.

`containment.json` lists no protected paths, so `gitWriteAllowed` refuses nothing in that worktree. A
project `settings.json` sets environment variables for every later command, and `.mcp.json` declares
servers that Claude Code starts outside the hook.

Fix: make these names an OS lock in every project root, independent of the `protected` list. Have
`Set-AgentProjectAccess` create admin-owned `.mcp.json`, `.claude\settings.json` and
`.claude\settings.local.json` if missing and deny the agent and sandbox accounts writing them. Limiting
`fetch` and `pull` to `origin` with no URL or path is worth doing too, but is not enough alone, because
`origin` also serves other people's pull-request refs.

### 2. Codex in a project: the hook checks little, and the sandbox can write what Claude Code trusts

These Codex shell commands pass in a project (tested through `Decide` with a Codex-shaped event):

| Command | Why it matters |
|---|---|
| `python build.py` | Any program runs. Intended: the sandbox is the lock. |
| `git status; python prior-art\x\run.py` | Untrusted code runs on the host. The first word is a reader, so `decideCodexShell` returns before looking at the rest (`hook/codex.go:78`). |
| `Set-Content .git\config …` | A `core.fsmonitor` entry runs when the agent account's Claude Code next runs `git status`, outside any sandbox and with network. |
| `Set-Content .mcp.json …` | Same pivot, through an MCP server Claude Code starts; if the owner opens Claude Code there, it starts as the owner. |
| `New-Item -ItemType Junction …` | Links inside the project. |

The first row is the design. The others break two stated rules: untrusted code never runs on the host,
and the Codex sandbox (network off) is a boundary. Code in the sandbox can leave it by writing a file
that an unsandboxed Claude Code session later acts on.

What I could not check: whether Codex's own sandbox already makes `.git` read-only, and what write
access it grants on the project when it runs. On disk today `CodexSandboxUsers` has read and execute
only on game's root.

Fix:

- Deny the sandbox accounts writing `.git`, `.claude` and `.mcp.json` in every project root, with the
  same lock as finding 1.
- In `decideCodexShell`, check every word for untrusted paths before the reader exemption, and apply
  the exemption only to a command with no `;`, `|`, `&` or newline.

## Medium

- **Codex events are recognized by a `turn_id` or `model` field** (`hook/hook.go:74`). If a Claude Code
  release adds `model` to its hook input, every Claude Code call silently gets the weaker Codex rules.
  Give Codex's hook command an argument (`agent-hook.exe codex`) and decide by that.
- **game's `.claude` is writable by the agent account at the OS level** (measured: inherited Modify).
  The response calls it the owner's, but it is not in game's `protected` list, so only the hook's name
  check holds it. Add it to the list.
- **`CLAUDE.md` is protected in game but does not exist**, so it has no deny entry and the agent account
  can create it. The response states this gap; creating each missing protected file empty and
  admin-owned would close it.
- **Codex's `requirements.toml` keys are unverified.** I cannot confirm from here that Codex reads
  `allowed_sandbox_modes`, `[windows] allowed_sandbox_implementations` or `windows_managed_dir` as
  written. Test by hand: `codex --yolo` and `--sandbox danger-full-access` are refused, and an
  escalation in game is denied by the hook.

## Low

- The deny entries on game's protected files appear twice for the Codex accounts in the `icacls`
  output. Check that the hourly task does not grow the access list on every run.
- The hourly SYSTEM task runs `icacls` on paths inside a tree the agent account can write. I tested
  the dangerous case: an inherited grant on a folder does not flow through a junction into its target.
  The explicit deny on a missing protected directory would follow a junction planted at its name, but
  it only adds a deny for the agent's own accounts.

## Confirmed fixed since `41dada9`

Through `Decide`:

- `git apply --unsafe-paths`, `cd -`, `git init --t=TPL` and `xxd -r -C hex TARGET` are each refused
  for their own reason.
- The xxd table matches the installed xxd (2024-02-10): `-n` and `-R` take values, `-C` does not.

By reading the code and tests:

- A reset cancels the call holding the VM before it waits.
- A call checks its client every 5 s and cancels when it is gone.
- A failed mid-run host check turns the VM off; one transient read failure is retried.
- The game disk's directories are owned from the drive root down.
- A make value holding shell syntax is refused whatever the pattern.

## The installed copy on this PC

- `agent-hook check`: every line OK. It reports this account as an administrator, so its shell and
  edit calls in a project will be refused in new sessions, as designed.
- `lab-broker check`: every line OK; lab state `game-lab`, Phase A, two disks. The game disk is not
  attached yet.
- `C:\ProgramData\agent-containment`, `projects` and `labs`: SYSTEM and Administrators full, Users
  read. `secrets` and `staging`: access denied to this unelevated account.
- game: `claude-agent` has Modify on the root; `Makefile`, `scripts` and `pull-prior-art.ps1` carry
  write denials for `claude-agent` and both Codex sandbox accounts.
- `C:\ProgramData\OpenAI\Codex\requirements.toml` is present and matches what the hook prints.

Still to do by hand, from the response's own list: the negative control with a network adapter,
`claude --dangerously-skip-permissions` from the agent account, and a push, pull and reset.
