# Response to the third review (`main` at `06da7c8`, installed, 2026-11-21)

Answers `REVIEW-2026-11-21-main-06da7c8.md`. Every fix is in the `review-3` pull request. The hook
findings are probes in `matrixProbes`. The install findings are checks in `install/common.tests.ps1`,
which CI runs and which runs the real `icacls` on a scratch tree.

The earlier response said the agent had no route to a commit holding `.mcp.json` or
`.claude\settings.json`. That was wrong: the lab and any fetch URL are both routes, as this review
shows. The fix no longer depends on there being no route. The names are locked by the OS.

## High

| # | answer | where | held by |
|---|---|---|---|
| 1 `.claude\settings.json` and `.mcp.json` through git | **Fixed by an OS lock.** In every project root, whatever its `protected` list says, `Set-AgentProjectAccess` denies the agent account and Codex's sandbox group writing `.mcp.json`, `.claude` (and everything under it), `.git\config` and `.git\hooks`. A missing `.mcp.json` (no servers) or `.claude` is created, admin-owned, first. A `git checkout FETCH_HEAD -- .claude/settings.json` now fails on the write. Also, as suggested: `git fetch` and `git pull` are refused except from `origin`, so a URL, a path or a bundle is refused by the hook. That alone is not enough, as the review says, which is why the lock comes first | `install/common.ps1` (`LockedNames`, `Set-AgentProjectAccess`), `fromOrigin` (`hook/bash.go`) | `common.tests.ps1`: each name exists and carries one deny; row 7 probes |
| 2 Codex writing what Claude Code trusts | **Fixed, by the same lock and the hook.** The OS lock above covers `CodexSandboxUsers`. In the hook: the reader exemption applies only to one plain command, so nothing joined with `;`, `|`, `&`, a new line, a backtick, `{` or `$(`. Without the exemption every word is checked for untrusted paths. `git` and `rg` are no longer readers for Codex, since `git -c core.fsmonitor=…` and `rg --pre` run programs. A command naming `.mcp.json`, `.claude`, `.git` or `GNUmakefile` is refused, and so is making a link (`New-Item -ItemType Junction`/`SymbolicLink`/`HardLink`, `mklink`). `python build.py` still passes, as designed: the sandbox is the lock | `decideCodexShell` (`hook/codex.go`) | row 24 probes: every command in the review's table, plus the forms that must keep working |

## Medium

| finding | answer |
|---|---|
| Codex recognised by `turn_id` or `model` | **Fixed.** Codex's hook command is `agent-hook.exe codex`, and the argument selects Codex's rules (`hook.DecideCodex`). `hook.Decide` is Claude Code's and never applies them. A test sends Claude Code an event with `turn_id` and `model`: `python build.py` in a project is still refused. Without the argument, a `PermissionRequest` is refused as not a PreToolUse event |
| game's `.claude` writable by the agent | **Fixed** by the locked names: `.claude` in every project root, so game's included, without listing it per project |
| `CLAUDE.md` protected but missing | **Fixed.** A missing protected path is created as an empty file, admin-owned, and locked like the rest |
| Codex's `requirements.toml` keys unverified | **Open, stated.** The keys are as Codex documents them, but have not been tried against Codex here. The hand test is in the doc's accepted exposures: `codex --yolo` and `codex --sandbox danger-full-access` must be refused, and an escalation in game must be denied by the hook |

## Low

| finding | answer |
|---|---|
| Duplicate deny entries | **Confirmed and fixed.** Each run added a full set (three on game's `Makefile`). Before adding, the deny entries of the agent account, the sandbox group and the two sandbox accounts earlier installs named are removed. The sandbox is now denied as its group, `CodexSandboxUsers`. `common.tests.ps1` runs it twice and counts one deny per name |
| The SYSTEM task acting on an agent-writable tree | **Tightened.** A link or junction at a locked or protected name, or in any folder between the project root and it, stops the run with the path named, and nothing is locked through it. `common.tests.ps1` plants a junction at `.claude` and checks its target gets no entry |

## Also found while answering

- `TestOwnerCommands` (`cmd/agent-hook`) built a binary "without a registry" that read the real
  registry path. It passed only because no readable registry existed on the machine running it, and
  it would have failed the next `make lab` on this PC. It now reads a directory that does not exist.
