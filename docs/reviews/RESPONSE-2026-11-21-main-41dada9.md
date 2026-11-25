# Response to the re-review of `main` at `41dada9` (2026-11-21)

Answers `REVIEW-2026-11-21-main-41dada9.md`. Every fix is in the `codex-v1` pull request. As the review
asked, each of the five bypasses is now a probe in `matrixProbes` (`hook/hook_test.go`), and each probe
was checked to be refused for its own reason, not an unrelated one.

## High: the five hook bypasses

| # | answer | where | held by |
|---|---|---|---|
| 1 `git apply --unsafe-paths` | **Fixed.** `apply` and `am` are no longer git writes: both apply a patch file the agent wrote, which can name any path. Both are refused as subcommands, and `--unsafe-paths` and `--directory` are refused options anyway | `gitWrites`, `refusedGitOptions` (`hook/bash.go`) | row 7 and row 10 probes |
| 2 `xxd -r -C hex.txt TARGET` | **Fixed.** `-C` moved to xxd's flags. The value options are now `l s c g o n R`, checked against xxd 2024's usage | `hook/options.go` | row 0 probes (`-C` with one operand passes, with two is refused) |
| 3 `git init --t=TPL` | **Fixed.** Any prefix of a refused long option from three characters up is refused (`--t`, `--ex`, `--op`). The comment now says what the code does | `refusedGitOption` (`hook/bash.go`) | row 10 probes: `--t=TPL`, `rebase --ex`, `grep --op=PROGRAM` |
| 4 `cd -`, `cd --`, `cd ~-` | **Fixed.** A `cd` argument starting with `-`, or with `~` other than `~`, `~/` and `~\`, is refused with the allowed form | `decideCd` (`hook/bash.go`) | row 6 probes, plus `cd ~/x`, which still passes |
| 5 a git write creating `.mcp.json`, `.claude\settings.json`, `GNUmakefile` | **Fixed for every route that can create one.** `am` and `apply` are gone (#1). `git mv` refuses a target named `.mcp.json` or `GNUmakefile`, or under `.claude` or `.git` (`refusedName`). The remaining git writes (checkout, merge, pull, …) can only bring in such a file if a commit already holds it, and the agent has no route that creates one: Edit and Write refuse these names anywhere (row 21), redirection is refused, and lab pulls land under `lab-out\`. Not taken: refusing git writes in every worktree that holds `.claude\`, which would end git writes in game, whose `.claude\` is the owner's | `refusedName` (`hook/edit.go`), `hook/bash.go` | row 7 probes |

## Medium

| finding | answer | where | held by |
|---|---|---|---|
| The ACL lock decays | **Fixed.** The deny entries are applied by `Set-AgentProjectAccess` (`install/common.ps1`). `setup.ps1` runs it on every `make lab`, and the hourly SYSTEM task `refresh-agent-claude` re-applies it, so a replaced file gets its entry back within the hour. The denials now also cover Codex's sandbox accounts if they exist. A protected path that does not exist yet still gets no entry until it exists. The hook refuses an edit of it by name meanwhile. Not done: `agent-hook check` reading the entries back. That needs ACL reads in Go, and is next | `install/common.ps1`, `install/setup.ps1`, `install/refresh-agent-claude.ps1` | not testable in CI (no admin); the install runs it |
| The agent shares the owner's desktop | **Stated** as an accepted exposure in `docs/agent-containment.md`. The planned fix is SSH to `localhost` as `claude-agent`: a separate, non-interactive session, and the same route lets the Claude desktop app work on projects | `docs/agent-containment.md` | n/a |
| `reset` cannot recover a stuck run | **Fixed.** The broker records the cancel of the call holding each VM, and a reset cancels it before it waits for the lock | `acquire` (`lab/broker.go`) | `TestLockAndLog`: a reset of a held VM cancels the holder and succeeds |
| A gone client is noticed only on a failed frame | **Fixed.** During a call the broker checks the pipe every 5 s with `PeekNamedPipe`, which reads nothing and fails once the client has closed its end. A blocking read was not used: the handle is synchronous, so a pending read would hold up the frame writes | `Answer`, `Connection.connected` (`lab/pipe_windows.go`) | `TestPipe`: a 60 s silent run is cancelled within the check interval after its client closes |
| A failed recheck leaves the VM running | **Fixed.** The broker turns the VM off (`Stop-VM -TurnOff`) and says so in the refusal. If turning it off fails, the refusal says that too | `run` (`lab/broker.go`), `HyperV.Stop` | `TestRunRecheckAndCancel`: the fake VM is stopped and `Off` |
| The game disk's parent directory | **Fixed.** `pack-game-disk.ps1` initializes every directory from below the drive root down as admin-owned (`D:\agent-containment` and then `D:\agent-containment\game`) | `install/pack-game-disk.ps1` | install-time |
| `mcp__claude_ai_` passes by name | **Answered, not changed.** A local server named `claude_ai_…` would need a new MCP configuration. Every place one can come from is closed to the agent: `~/.claude.json` and `~/.claude` settings and plugins (protected), `.mcp.json` (refused by name, by every route above), the managed directory (admin), and `claude mcp add` (not an allowed program). `allowManagedMcpServersOnly` was not set: it would also disable the owner's own local MCP servers | `hook/hook.go` | row 18 and row 21 probes |

## Low

| finding | answer |
|---|---|
| `^x.*$` admits `x;calc` | **Fixed.** A make value holding shell syntax or white space is refused whatever the registry's pattern says (`makeForm`). `TestLooseMakePattern` sets the pattern to `^x.*$` |
| Device cmdlets with `SilentlyContinue` | **Fixed.** A cmdlet this Windows lacks counts as no devices; any other error fails the read, and so the gate |
| A transient `Info` error cancels a long run | **Fixed.** Mid-run, one failed read is retried at the next tick and two in a row stop the run. Before a run, one failed read still refuses. `TestRunRecheckAndCancel` covers both |
| `refusedGitOption`'s comment | **Fixed** with #3 |
| Running `update.ps1` from a working clone later | **Stated** in the README: after the first install, run only the installed copy (`make lab` does) |
| `MultiEdit` in `editTools` | Kept, deliberately: harmless, and it covers older Claude Code versions the owner may still run |

## Also in this pull request (not from the review)

- **Codex** (owner, 2026-11-21: allowed in projects, sandboxed): the admin-owned
  `C:\ProgramData\OpenAI\Codex\requirements.toml`, the same hook for Codex's `PreToolUse` and
  `PermissionRequest`, and `agent-hook check` lines for both. See "Codex" in `docs/agent-containment.md`.
- **The agent's Claude Code refresh**: an hourly SYSTEM task that installs a newer, Anthropic-signed copy
  of the owner's Claude Code for the agent account without running it.
