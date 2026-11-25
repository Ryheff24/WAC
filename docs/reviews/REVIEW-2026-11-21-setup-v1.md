# Review: agent containment (branch `setup-v1`, 2026-11-21)

**Verdict: not ready to rely on.** The lab design (no adapter, SYSTEM broker, host-side gate) is sound,
but the broker has a write-as-SYSTEM hole that breaks the "lock" rows. The hook's allowlist can also be
bypassed with one-line commands.

Scope: all the Go, the install scripts and both docs, at commit `12adf3d`. `go vet` and the suite pass.
The containment was not installed on the PC at review time (`C:\Program Files\agent-containment` and
`C:\ProgramData\agent-containment` did not exist), so this is a source review. Items marked **tested**
were reproduced with harmless commands in a scratch directory against the real bash, git 2.45.1 and
sort; the hook binary was never run against them. The rest are from reading the code.

## Critical: breaks a "lock"

### 1. `pull` writes as SYSTEM into a directory the agent controls

`lab/broker.go:177-181`.

- The destination is `<project>\lab-out\<UTC second>`, created with `MkdirAll`, with no link check.
- Any process running as the owner can pre-create that name as a junction to
  `C:\ProgramData\agent-containment\projects` or `C:\Program Files\agent-containment`.
- `Copy-Item -Force` then drops a guest file of the agent's choosing there: a new `game.json`, a lab
  state, or `agent-hook.exe` itself.
- `markUntrusted` runs only after the copy.
- Fix: impersonate the pipe client for all host file I/O, create the destination with an exclusive
  `Mkdir`, and refuse reparse points on the way.

### 2. `push` reads as SYSTEM with a check-then-use gap

`lab/broker.go:160-163`. A directory can be swapped for a junction between `noReparsePoints` and
`Copy-Item`, which gives the agent a read of any file SYSTEM can read. Impersonation fixes this too.

### 3. The install trusts directories and a PATH the agent can prepare

- `C:\ProgramData\agent-containment`, `C:\agent-containment\vms` and `D:\agent-containment\game` are in
  places any user can create first. `install.ps1` and `new-lab.ps1` reset the ACL but never the owner
  or existing children, and an owner can always re-grant itself.
- A pre-planted `labs\*.json` is kept; only `projects\*.json` is cleared.
- `pack-game-disk.ps1` sets no ACL on the game disk's directory, so the size and write-time check on
  the parent can be forged.
- The elevated half of `update.ps1` and `setup.ps1` runs `pwsh`, `git` and `go` by name. This PC's PATH
  has user-writable directories (`~\bin`, scoop shims, WinGet Links, which is where `make` lives).
- The elevated `git` and `go` read the user's `~/.gitconfig` and `%APPDATA%\go\env`. A `url.insteadOf`
  or `GOFLAGS=-toolexec` entry there becomes elevated code.
- Fix: absolute program paths, a fixed PATH, `GIT_CONFIG_GLOBAL=NUL`, `GOFLAGS`/`GOENV=off`,
  `icacls /setowner … /T`, and refuse a pre-existing directory not owned by Administrators.
- `registry/registry.go:22` says `check` measures the ACL. It does not.

### 4. The OS "locks" assume the agent's account is not an administrator

If the owner's daily account is a UAC split-token admin, UAC is not a security boundary. The account's
groups could not be read during the review. The doc should state the requirement and `check` should
fail on it.

## High: hook bypasses in a project session

All are in the Bash path.

| # | Bypass | Status | Where |
|---|---|---|---|
| 5 | A `#` comment hides a quote: `true # '` then a second line `anything # '`. Bash runs line 2; the parser sees one quoted word. | tested | `hook/shell.go:75` |
| 6 | `echo x >&2/../path` and `>&1name` write a file; the parser accepts `>&1`/`>&2` without checking what follows. Any protected file can be overwritten. | tested | `hook/shell.go:133` |
| 7 | Brace expansion and `$'\x2d…'` are not modelled: `sort {-o,Makefile} in` and `sort $'\x2do' f in` pass `refuseOptions`. Same for `find {-exec,…}` and `git rebase {-x,…}`. | tested | `hook/shell.go`, `hook/bash.go:49` |
| 8 | Attached and abbreviated options: `git grep -O<program>` ran `wc`; `sort --outp=file` wrote the file; git accepts `--exe` for `--exec` and `--upload-pa` for `--upload-pack`. | tested | `hook/bash.go:194`, `hook/bash.go:256` |
| 9 | A directory holding `HEAD`, `config`, `objects/`, `refs/` is a repository. Its `core.fsmonitor` ran on `git -C x status`. The Write tool can create all of it; only a `.git` component is refused. | tested | `hook/edit.go:46` |
| 10 | Other spellings of a protected file reach the same file: `Makefile::$DATA`, `C:Makefile`, and short names (`GITCON~1` is `.gitconfig`, `GIT~1` is `.git`, `CLAUDE~1` is `.claude`). | tested with Node's `fs`, not the Write tool | `hook/edit.go:23`, `registry/registry.go:198` |
| 11 | `gitWriteAllowed` checks the cwd, not the repository's top level. From `game\src`, `git am` or `git checkout` rewrites the protected root `Makefile`, which the `make` row then runs. | read | `hook/bash.go:320` |
| 12 | `go vet -vettool=PROG` is not refused; `go build -o` can write an `.exe` anywhere; `go test` runs code the agent wrote. `host_go` means arbitrary host code, which the doc's "no interpreters" statement contradicts. | read | `hook/bash.go:475` |
| 13 | `rg --hostname-bin=COMMAND` (present in the installed rg 15.1) is not refused. | option confirmed, not run | `hook/bash.go:31` |
| 14 | Unprotected start-up files: `~/.claude/shell-snapshots/*.sh` exist on this PC and the Bash tool appears to source them (not documented); `%APPDATA%\.curlrc` and `_curlrc`; the Startup folder for types outside `runnable` (`.py`, `.url`). | read | `hook/hook.go:163` |

For 10, `registry/registry.go:197` cites `hook.sameOrBelow`, an identity comparison that does not exist.

### Suggested shape of the fix

- **Parser:** refuse what it does not model (`{`, `$'`, `$"`, unquoted `#`, glob or `$` words in option
  position), and require a delimiter after `>&N`.
- **Options:** switch each program from refused options to allowed options, as `gitClone` and
  `curlForm` already do.
- **Paths:** resolve by file identity; refuse `:` past the drive, `~digit` components and trailing dots
  or spaces; make `~/.claude` default-deny with `memory` excepted.

## Medium

- **Matcher coverage.** The matcher names seven tools. MCP tools and anything new run with no hook, and
  `MultiEdit` is no longer a built-in. Per the docs (checked 2026-11-21), `*` matches all. Use it and
  have the hook pass a known list. Consider `allowManagedMcpServersOnly` and
  `strictPluginOnlyCustomization`.
- **`onFailure: "block"` is documented** (v2.1.295+). The docs do not name
  `--dangerously-skip-permissions` under `disableBypassPermissionsMode`, so test that once by hand.
- **The in-guest probe is not a lock.** Untrusted code runs as the guest administrator and can forge
  the hostname, the loopback result and "nothing reached". Only the host-side topology check holds. The
  layers table should say so.
- **Topology gaps.** The gate does not check COM ports, DVD drives or assigned devices (a passed-through
  NIC). It is not re-run during a run of up to 24 hours.
- **GPU partitioning** exposes the host GPU driver to the guest. It is an owner decision but missing
  from the accepted exposures.
- **The lab's ISO is "the newest `*.iso` in `game\downloads`"**, the one directory `curl -o` may write.
  Only `setup.exe`'s signature is checked, not the image. Pin a SHA-256 in `registry/labs/*.json`.
- **One lock per VM, and an abandoned run holds it.** A `lab-run` that is killed leaves the guest run
  going, because the context is the server's and write errors are ignored. `status`, `push` and `reset`
  then wait up to the timeout, an hour by default.
- **Pipe ACL.** `GRGW` gives the owner the right to create pipe instances, and there is a moment with
  no listening instance between connections. `lab-run`'s server-image check limits this to denial of
  service.
- **Outside a project**, `cd game && python x.py` from the parent directory passes: `reaches` only
  looks at words containing a slash. The doc calls this a tripwire; resolving bare words would close it
  cheaply.
- **Protect `main` on GitHub.** The push rule is a tripwire and the credential is available to any
  bypass.

## Low

- `lab-run -- game.exe` through Bash from outside a project is refused by `firstGame`, though the
  refusal text recommends exactly that form.
- `git config --list` with extra arguments passes on the presence of the flag alone.
- `agent-hook check` compares three keys and would not notice `disableAllHooks` or a
  `managed-settings.d` file.
- The broker log file name is fixed at start and never rolls; `hostAddresses` returns nothing on error.
- `decideEdit` points to a scratchpad `window\` procedure in `docs/agent-containment.md` that the doc
  does not describe.
- `readerChecks` has an entry for `tree`, which is not a reader.

## What held up

- `git fetch ext::…` is refused by git itself (tested).
- `git log --outp=` is not accepted as an abbreviation (tested).
- `gitClone`, `curlForm` and `makeForm` are true allowlists and resisted the tricks above, apart from
  the brace case when the cwd is the downloads directory.
- Request data never becomes script text in the broker.
- Registry and state parsing is strict and fails closed.

## Order to fix in

1. Items 1–3: impersonation in the broker, and install hardening.
2. The parser (5–7).
3. Allowlisted options per program (8, 12, 13).
4. Path identity (9–11, 14).
5. Matcher `*`.
6. The doc's lock and tripwire table.

Each tested bypass should become a row in `TestForms` or `TestEditPaths` first; none is covered today.
