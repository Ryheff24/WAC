# Decisions

Newest first. Each says who decided, when, and why.

## 2026-11-23: the gate keeps 10 GiB free under a lab's disks; reset grants its new disks to the VM

Reported by the game session.
- **The guest filled the host's C: by rewriting its game volume.** game-diff.vhdx grew by tens of
  GB in one boot with no new files, probably from Windows' scheduled drive optimisation. Host C:
  reached 0, the VM paused and WSL crashed. Now the host check also requires 10 GiB free on every volume
  holding an attached disk, so a VM below that never starts, and a run's recheck turns it off. This is
  under the push reserve (20 GiB), so a push never leaves the lab unable to run. A rebase disables the
  guest's Defrag tasks, the `defragsvc` service and NTFS last-access writes in the new system disk, and
  checks them again after a reboot. On the first rebased guest the task alone came back on about 10
  minutes after boot and wrote ~244 GB to the work disk before it was stopped. Keeping the game volume
  read-only in the guest is the game side's, sealed in with `-Seal`.
- **The broker watches running labs between calls.** The first rebased guest re-enabled its Defrag
  task by itself and wrote ~244 GB to the work disk while no call was running, so the per-run recheck
  never saw it. Every minute, the broker now turns off any running lab VM whose host check fails.
- **`reset` left the VM unable to start** ("Account does not have permission to open attachment"):
  Hyper-V grants a disk's file to the VM's account (`NT VIRTUAL MACHINE\<VM id>`) only when it
  attaches the disk, and a reset swaps new files in under paths already attached. Each new child is now
  granted before it is swapped in. `rebase-os.ps1` grants every disk it reattaches, so a rebase also
  repairs a VM a reset broke.

## 2026-11-23: an Evaluation guest is rebased every ~90 days, activated online with nothing else attached (owner)

game-lab's Windows 11 Enterprise Evaluation was never activated, because the lab has never had a
network. After the grace period Windows shuts the guest down an hour after every boot, and a rearm did
not help. The owner chose the free route over a paid license: a fresh Evaluation system disk, activated
online once and then sealed, every ~90 days (`install/rebase-os.ps1`). The owner's hard rule: the game
disk and the work disk, which may hold a quarantined sample, are never in a VM that has a network
adapter. The script asserts it at every phase, and the adapter exists only while the new, blank system
disk is the VM's only disk.
- **Built in the lab VM itself:** activation is bound to the VM's virtual hardware (its BIOS GUID and
  the like), so an activated disk moved to another VM would likely need activating again, offline.
  This is believed, not tested.
- **Applied offline, as the lab was first built,** not installed by hand from the ISO in a DVD drive:
  the guest's name and account (with the password already in `secrets\`) come from an answer file, so
  the owner types nothing, and the VM never has a DVD drive.
- **Same paths, no state change:** the old disks are moved aside, and the new ones take their paths, so
  the lab state is never written, and rollback is moving files back.
- **The broker's task is disabled during a rebase,** although the gate would refuse the VM anyway, so
  that no call starts or resets it between steps.
- **Installed beside the broker:** `install.ps1` copies the script from its fresh clone into
  `C:\Program Files\agent-containment\`, and it refuses to run from anywhere else.

## 2026-11-21: the Windows side is only the lab; agents run in WSL's sandbox (owner)

The first Windows containment (the PreToolUse hook, the `claude-agent` account over SSH, the hourly
refresh, the ACL locks, the Codex requirements, the bootstrap/setup/update chain) was uninstalled by
the owner as too complex, and is deleted here. Claude Code's sandbox in WSL2 is the wall now (`wsl/`).
What stays on Windows is the lab: `lab-broker` (SYSTEM, one scheduled task) and `lab-run.exe`, built and
installed by one elevated `install\install.ps1`, removed by `uninstall.ps1`, never touching the VM.
- **The pipe admits the owner's account**, not an agent account: `lab-run` runs as the owner, launched
  from WSL. `install.ps1` records it with `lab-broker set-owner` (a Go JSON round trip, so the game
  disk's recorded write time is kept to the 100 ns; PowerShell's would not be).
- **Arguments cross from WSL base64-encoded** (`lab-run.exe --wsl b64:...`): a Windows command line is
  one string the program splits again, and WSL's quoting of quotes and backslashes is not something to
  rest a boundary's argv on.
- **The WSL wrapper is a static Go binary, not a shell script** (review of this change): it is the one
  program outside the sandbox, and a dynamically linked `sh` would load whatever `LD_PRELOAD` named if
  a caller's environment ever reached it.
- **Transfers go through a lab exchange on Windows** (`C:\Users\<owner>\agent-lab\<project>`), not
  through `\\wsl.localhost`: the broker reads and writes as the caller, only on NTFS, where it sees
  links and junctions, as it was tested. The sandbox may write only the exchange's `lab-in`; nothing
  there is protected or runs on the host. `lab-run` itself writes nothing and runs no host program:
  `sync` takes a bundle the agent made in its sandbox.
- **The registry keeps only what the lab needs:** name, lab VMs, the exchange (`root`), `wsl_root`.
  The VM-building scripts (`new-lab.ps1`, `pack-game-disk.ps1`, `seal-lab.ps1`) went with `setup.ps1`:
  the lab is never rebuilt by an install, and they are in the history.

## 2026-11-21: answers to the first two outside reviews

Every finding is answered, and nearly all are fixed, in `reviews/RESPONSE-2026-11-21.md`. The decisions
in it:
- **Agents run as a standard account** (owner). UAC is not a security boundary, so `claude-agent` is not
  an administrator. Its protected files are write-denied by ACL, and the hook refuses an administrator's
  shell and edit calls in a project. The owner stays signed in as themselves, and a `runas /savecred`
  shortcut starts the agent's Claude Code.
- **Nothing agents write runs on the host:** host Go is gone, and builds and tests run in the lab.
- **The host check is the lock; the guest probe is a diagnostic.** A start or reset happens only after
  the host check passes. Phase B is refused until its topology is proved.
- **SYSTEM never touches a host path on the caller's word:** transfers go as the caller through staging.
- **Readers are allowlists of options,** not lists of refused ones.
- **Elevated install code comes only from a fresh clone of main and a trusted toolchain.** The first
  install is the stated exception.
## 2026-11-21: the lab's route in is a SYSTEM broker on a named pipe, over PowerShell Direct

- **Why a broker, not the agent in Hyper-V Administrators:** that group could add a network adapter.
  The broker holds the Hyper-V rights, answers five verbs, and proves the gate every call.
- **Why PowerShell Direct:** it uses VMBus, so the VM needs no network adapter at all.
- **Why a scheduled task as SYSTEM, not a Windows service:** a Go service needs `golang.org/x/sys`.
  The task keeps the boundary standard-library only, like the rest of the module.
- **Why copies, never host-mounted guest disks:** the host's NTFS driver would parse a file system the
  guest wrote.
- **Pipe squatting:** the pipe's first instance is created with `FILE_FLAG_FIRST_PIPE_INSTANCE`, and
  `lab-run` checks the server's program path.
- **Pulled files are marked as from the internet:** Mark of the Web, `Zone.Identifier` ZoneId 3.
- **Phase B (a server VM on a Private switch) is modeled in the gate and the state, and not yet built.**

## 2026-11-21: the VM is applied offline, not installed

`new-lab.ps1` applies the owner's Windows image with DISM onto a new virtual disk and adds an answer
file in `Windows\Panther`. That means no Windows Setup, no network during OOBE, and nothing to click.
`seal-lab.ps1` later folds the provisioned guest into the base disk, so a reset returns to a ready VM.

## 2026-11-21: CI and coverage as d1pres has them (owner)

The same coverage workflow, gate and octocov setup as d1pres: the suite with coverage on every PR
and on main, built binaries measured through `-cover` and GOCOVERDIR, a gate at 60% total with no
file at 0%, failing tests posted on the PR, and octocov's comment with Patch Coverage. Reports stay
in this repository's Actions artifacts. Differences: the suite runs on a Windows runner and octocov
in a second Linux job, because the hook decides Windows paths and calls the Windows registry API.

## 2026-11-21: agents push feature branches; main is the owner's

The owner asked agents to push to this repository. The hook allows `git push [-u] origin BRANCH`
in projects with `push_branches` (only this repository today): never main or master, never forced,
never deleting. main changes only by a pull request the owner merges, and install builds main.

## 2026-11-21: the core stays Go and C (owner)

Go for everything outside the game process (hook, broker, launcher, install tooling), C for the
in-process game gate, compiled into every binary that runs game code and every test binary.
Reason (from d1pres): the owner must be able to read the boundary they are trusting. When the game
comes, it gets the same checks as d1: it proves it is contained before it runs.

## 2026-11-21: one containment for every project (owner)

The game project (`game`) first, d1preservation on Windows later. Nothing in the code names a project. The registry in
`C:\ProgramData\agent-containment\projects\` holds each project's root, untrusted trees, protected
paths, lab VMs, game binary names and make targets.

## 2026-11-21: default deny on the host, because native Windows has no Claude Code sandbox

Measured from the docs (code.claude.com/docs/en/sandboxing.md): the sandbox runs on macOS, Linux and
WSL2 only. d1's hook passes sandboxed commands and polices the sandbox-off form. Here there is no
sandbox to pass anything to, so the hook allows a list of forms and refuses the rest.

Considered and not taken: running Claude Code inside WSL2 to get its sandbox. WSL on this PC hung
(`wsl -l` did not return), and a WSL sandbox would still need the same VM boundary for untrusted code.

## 2026-11-21: hooks fail open unless told not to

From code.claude.com/docs/en/hooks.md: a PreToolUse hook that cannot start, or times out, lets the
call through, and only exit 2 blocks. So the managed hook entry carries `"onFailure": "block"`
(Claude Code 2.1.295 or later), and the hook exits 2 on every error, a Go panic included.

## 2026-11-21: Windows 11 lab VM, no network adapter, GPU partitioning from the start (owner)

Agents may run anything inside the VM, but only through `lab-run`. Game data stays on an SSD.

## 2026-11-21: Codex is allowed in projects, sandboxed (owner)

Codex (CLI and desktop app) runs its commands in its own Windows sandbox (a low-privilege account,
network off). The admin-owned `C:\ProgramData\OpenAI\Codex\requirements.toml` blocks, machine-wide,
only full access, `--yolo` and approval `never`, plus the restricted-token sandbox (no network
isolation). Computer use, browser use, MCP servers and web search stay as the owner has them (owner's
choice). In a project the managed hook refuses every escalation, so Codex commands there stay in the
sandbox. Codex hooks fail open, so the sandbox is the lock and the hook the tripwire.

## 2026-11-21: the agent's Claude Code follows the owner's, by signature

Claude Code updates itself often. The agent account cannot write its own copy, so a SYSTEM task copies
the owner's every hour and installs the copy only if it is validly signed by Anthropic, PBC, newer
than the installed one and at least 2.1.295. It never runs the owner's file. The same task re-applies
the protected files' write denials.

## 2026-11-21: git am and git apply are refused

Both apply a patch file the agent wrote, which can name any path, outside the repository included
(`--unsafe-paths`) or a file an edit may not create. Nothing the agents do needs them; `git cherry-pick`
and `git merge` work from commits.

## 2026-11-21: some names are locked in every project root, whatever the project says

`.mcp.json`, `.claude`, `.git\config` and `.git\hooks` are what Claude Code or git act on without
asking: MCP servers start outside the hook, `.claude` settings set variables for every command, and
git config and hooks run programs. The hook's name checks cover the agent's own edits, but a git fetch
or Codex's sandbox could write them another way (review of `main` at `06da7c8`). So the install
creates any that are missing, admin-owned, and denies the agent account and Codex's sandbox group
writing them, in every project root. git fetch and pull are also limited to origin.

## 2026-11-21: Codex's hook is told it is Codex's

Codex's hook command is `agent-hook.exe codex`. Recognising Codex by the fields of its events would
have given every Claude Code call Codex's weaker rules the day Claude Code added such a field.
