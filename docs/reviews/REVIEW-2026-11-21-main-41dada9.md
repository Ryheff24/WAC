# Re-review: agent containment (`main` at `41dada9`, 2026-11-21)

**Verdict: the locks now hold up on reading; the hook is still bypassable in five tested ways.** The
critical findings of the first review (`REVIEW-2026-11-21-setup-v1.md`, at `12adf3d`) are fixed: the
broker no longer touches host paths as SYSTEM, the install refuses planted directories and uses a
trusted environment, and agents run as a standard account. What remains is in the hook's command
rules, plus a few gaps in the new account and broker work.

Scope: every change since `12adf3d` (the Go, the install scripts, `RESPONSE-2026-11-21.md`, the doc).
The suite passes. Nothing is installed on this PC, so the Hyper-V, ACL and Claude Code policy parts are
still unverified on a real machine, as the response says.

How the hook findings were tested: each command was run through `hook.Decide` in a scratch copy of
`41dada9`, using the suite's own `testConfig`, and passed. The effect of each was then reproduced with
harmless commands against the real bash, git 2.45.1 and xxd.

## High: hook bypasses that still pass (all tested)

| # | Command that passes | What it does | Where |
|---|---|---|---|
| 1 | `cd C:/Temp/work; git apply --unsafe-paths p.patch` | Outside a repository, `--unsafe-paths` lets a patch write `../` paths. It rewrote a file outside the cwd. Any file the agent account can write is reachable: `~/.gitconfig`, a repository's `.git/config`, `~/.claude/settings.json`. | `gitWrites` in `hook/bash.go:154` |
| 2 | `xxd -r -C hex.txt TARGET` | The spec lists `-C` as taking a value; xxd's `-C` takes none. The hook skips `hex.txt` as the value and counts one operand; xxd sees two and writes `TARGET` with any bytes. | `hook/options.go:64` |
| 3 | `git init --t=TPL r; git -C r status` | Refused prefixes need five characters, but `--t` and `--te` are unambiguous for `--template`. The template's `config` set `core.fsmonitor`, which ran on `status`. Same gap: `git rebase --ex CMD` and `git grep --op=PROGRAM`. | `hook/bash.go:168` |
| 4 | `cd A; cd B; cd -; git status` | The hook resolves `-` as a directory named `-` under B; bash goes back to A. Also `cd --` (bash goes home) and `cd ~-`. Every later check in the command (`gitTop`, make's root, curl's target) is made against the wrong directory. | `decideCd`, `hook/bash.go:128` |
| 5 | `git am x.patch` in the containment repository | A git write can create the files Edit refuses by name: `.mcp.json`, `.claude/settings.json` (its `env` block sets variables for every later command), `GNUmakefile`. `gitWriteAllowed` only asks whether the worktree holds a protected path, and `containment.json` lists none. | `hook/bash.go:367` |

Fixes:

- **1:** drop `apply` from `gitWrites` (keep `am`, which works from the worktree top), or allowlist its
  options.
- **2:** move `C` to xxd's flags. More generally, add a test that runs each spec's value options
  against the real program, because a wrong arity in the table is a silent bypass.
- **3:** refuse any prefix of a refused long option (three characters and up), or better, allowlist
  options per git subcommand as the readers now do.
- **4:** refuse a `cd` argument that starts with `-` or `~` (other than `~/`).
- **5:** treat `.mcp.json`, `.claude\`, `GNUmakefile` and `.git` as protected in every project root, so
  `gitWriteAllowed` refuses worktrees that would hold them; or protect them by ACL as the project's
  protected paths are.

## Medium

- **The ACL lock on protected files decays.** `new-agent-account.ps1` puts the deny entry on each
  protected file that exists when it runs. A file the owner later replaces (an editor's save-by-rename,
  `git pull`) is a new file without the entry, and a protected path that does not exist yet gets none.
  Nothing re-applies or checks it. Put the entries in `setup.ps1` (every update) and have
  `agent-hook check` verify them.
- **The agent account shares the owner's desktop.** The shortcut starts it with `runas` in the owner's
  session. Code running as `claude-agent` can send keystrokes to the owner's unelevated windows, a
  terminal included; that is not blocked between accounts at the same integrity level. It needs a hook
  bypass first, so it weakens the account layer rather than opening a hole by itself. State it in the
  accepted exposures, or run the agent in its own sign-in session.
- **`reset` cannot recover a stuck run.** The busy message says "lab-run reset if a run is stuck", but
  reset waits for the same VM lock and gets the same refusal after two minutes
  (`lab/broker.go:86`, `:266`).
- **A gone client is noticed only when a frame fails to deliver.** A run that prints nothing holds the
  VM until its timeout, an hour by default. Nothing reads the pipe after the request; a blocked read
  would see the close (`lab/pipe_windows.go:222`).
- **A failed recheck leaves the VM running.** If the host check fails mid-run (an adapter appears), the
  broker stops relaying and reports `REFUSED during the run`, but the guest workload keeps running with
  the adapter. Turn the VM off there (`lab/broker.go:205`).
- **The game disk's parent directory.** `pack-game-disk.ps1` takes ownership of `D:\agent-containment\game`
  only. `D:\agent-containment` inherits the drive root's access list, which on a data drive normally
  lets any signed-in user rename it while the VM is off and put another `game.vhdx` at the path. The
  size and write-time check would not notice a forged pair. Initialize each directory down from the
  drive root, as `vm_root` now is under `ProgramData`.
- **`mcp__claude_ai_` passes by name.** `mcp__claude_ai_evil__run` passed in a project session. If a
  locally configured MCP server can be named `claude_ai_…`, its tools skip the hook. I did not check
  whether Claude Code reserves the prefix. `allowManagedMcpServersOnly` in the managed settings would
  settle it either way.

## Low

- `checkValuePattern` tests a few sample strings, so `^x.*$` is accepted and admits `x;calc`. Have
  `makeForm` refuse the shell-syntax characters in the value itself, whatever the pattern.
- `Get-VMAssignableDevice` and `Get-VMFibreChannelHba` run with `-ErrorAction SilentlyContinue`, so an
  error counts as zero devices.
- A transient `Info` error during the minute recheck cancels a long run.
- `refusedGitOption`'s comment says "four characters or more"; the code says five.
- The first install, and anyone running `install\update.ps1` from a working clone later, elevates
  scripts from a tree the agent account has Modify on. The response states the first-install case; the
  README could say to use only the installed copy afterwards.
- `MultiEdit` is still in `editTools`, though it is no longer a built-in tool. Harmless.

## Fixed since the first review

Checked by reading the code and its tests:

- **Broker:** pull and push work through an admin-only staging directory and do host I/O as the pipe
  client; pull publishes into an exclusively created directory. The pipe's client rights exclude
  creating instances, and a spare instance always exists.
- **Gate:** the host check runs before any start or reset, again after a start and every minute of a
  run; it requires exactly the recorded disks and parents and refuses DVD drives, COM ports, assigned
  devices and Fibre Channel. Phase B is refused outright. The probe is labeled a diagnostic.
- **Parser:** unquoted `#`, braces, `$'…'`, `$"…"`, and `>&N` without a delimiter are refused. Words from
  variables or patterns are refused outside echo, printf, test and cd. `${NAME:=value}` is covered by
  the brace rule (confirmed).
- **Readers:** every reader is an allowlist of options; `rg --hostname-bin`, `sort --outp` and
  `find -exec` are refused.
- **git:** hand-made git directories are refused and the install sets `safe.bareRepository=explicit`;
  writes are checked against the worktree top; config reads are an exact form; `-O` and `-x` are
  refused in any bundle.
- **Edits:** streams, drive-relative paths, trailing dots and spaces, UNC paths, links and junctions are
  refused; short names are expanded first; `~\.claude` is default-deny except memory and plans.
- **Tools:** the matcher is `*`; unknown tools are refused in a project; `go` is refused on the host.
- **Install:** fixed program paths and PATH, no global git config, no go env file, admin-only caches;
  planted directories are refused and ownership is set and checked; the ISO is pinned by SHA-256 and
  hashed from an admin-only copy; failed checks fail the install.

## Order to fix in

1. Findings 1 to 4: each is a one-line command today.
2. Finding 5 and the decaying ACL, which together decide whether protected files are locked.
3. The broker's stuck-run items.
4. The rest.

Each of findings 1 to 5 should become a probe in `matrixProbes`; none is covered today.
