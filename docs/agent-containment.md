# Agent containment: the rule, the two places, the way between them

The containment for every project on this PC (a game project, `game`, first). It keeps d1pres `containment/`'s core: **Go
outside the game process, C inside it**, so the owner can read the boundary they are trusting.

## The rule

- **Agents keep their network; untrusted code never has one.** Prior art, game dumps, game code and
  their tests run only inside a Hyper-V lab VM with no network adapter.
- **Agents do not change what governs them.** Instructions, the Makefile, scripts, `.claude`, `.git`
  and the containment itself are the owner's.
- **The owner installs; agents never do.** Changes land here by pull request; both installs build from
  a fresh clone in a directory only root or Administrators can write.

## The two places

| place | who runs there | what holds it |
|---|---|---|
| WSL2 (Ubuntu), projects on ext4 (`~/projects/...`) | Claude Code, as the owner's Linux account | Claude Code's sandbox (bubblewrap), locked on by root-owned managed settings, plus `edit-hook` for the file tools (`wsl/`) |
| the lab VM (`game-lab`) | untrusted code, as the guest's account | Hyper-V: no network adapter, proved by the SYSTEM broker before every call |

Projects stay on ext4, never under `/mnt/c`: on a Windows drive the sandbox's write denials can be
walked around by other spellings of a name (case, 8.3 short names).

What sandboxed commands can read: nothing on the Windows drives (all of `/mnt` is denied, as a whole,
because a denial of one path under `/mnt/c` could be reached by another spelling of it) except each
project's lab exchange, and none of the home's credentials (`~/.ssh`, `~/.claude/.credentials.json`,
`~/.config/gh`, `~/.aws` and the like: `CredentialPaths` in `wsl/edithook/settings.go`). The network is
open to GitHub, so anything readable could be uploaded. Only the managed settings can re-open a read.

`edit-hook` (the Edit and Write tools, which the sandbox does not cover) holds a registered project's
protected paths, untrusted trees and instruction files (`CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md`,
`.claude`, `.mcp.json`, `.git`, at any depth) against every session, wherever it was started, and a
link is judged by where it leads, even when its target does not exist yet. In the project's own session
it also keeps writes inside the project, its scratchpad and its memory.

Shell commands are held by the sandbox's `denyWrite`, which names the protected paths at the project
root only (`protected` in `wsl/projects.json`). So from a shell, a nested `CLAUDE.md`, `AGENTS.md` or
`.mcp.json` below the root is writable; Claude Code loads a nested `CLAUDE.md` only when it works in
that directory. List any directory that must stay instruction-free under `protected`.

The one way from the first place to the second is `lab-run`. In WSL it is `/usr/local/bin/lab-run`
(`wsl/labrun`, a static binary, root-owned), the one command the managed settings let run outside the sandbox. It
runs the Windows `lab-run.exe`, which asks the SYSTEM `lab-broker` over a named pipe. Nothing else in
the sandbox reaches Windows (the WSL interop socket is outside it).

## From WSL to the lab

- **Arguments arrive exactly.** The wrapper base64-encodes every argument and its working directory
  (`lab-run.exe --wsl b64:CWD b64:ARG...`), so no quote, space or backslash is re-read on the way into
  a Windows command line. Exit codes come back unchanged (the guest program's, 2 for a refusal, 64 for
  a usage error; WSL keeps the low 8 bits).
- **Nothing of the caller's runs outside the sandbox.** The wrapper is a static Go binary, so there is
  no dynamic loader for `LD_PRELOAD` and its kind to reach, and it gives `lab-run.exe` a fixed
  environment: `PATH` and the two variables interop needs (`WSL_INTEROP`, `WSL_DISTRO_NAME`); no
  `WSLENV`, so no variable crosses to Windows.
- **Agents call it as `/usr/local/bin/lab-run ...`, by its full path, as a command on its own.** Only
  that form runs outside the sandbox, which it needs to reach Windows. The rest stay sandboxed and fail
  on WSL's interop socket. That includes bare `lab-run`, an `LD_PRELOAD=` or `env` prefix, and any
  compound line (`;`, `&&`, `|`, `> file`, `$(...)`, `<(...)`), which was tested live on 2026-11-21.
- **Arguments reach the guest program exactly.** The broker joins them into one Windows command line by
  the CommandLineToArgvW rules, and the guest passes it to the program verbatim. Windows PowerShell 5.1's
  own joining drops embedded quotes. A cmdlet, function or script gets them as PowerShell arguments.
- **The project** is the registered one whose `wsl_root` holds the working directory (or `--project`).
- **Files go through a lab exchange on Windows**, the project's registry `root`
  (`C:\Users\<owner>\agent-lab\<project>`): the broker reads and writes host files only as the caller
  and only on NTFS, where it can see links and junctions. The WSL sandbox may write that folder's
  `lab-in\` and nothing else on Windows (`lab_in` in `wsl/projects.json` becomes its `allowWrite`):
  - `push`: copy the file into `/mnt/c/Users/<owner>/agent-lab/<project>/lab-in/` (sandboxed `cp`),
    then `/usr/local/bin/lab-run push '/mnt/c/.../lab-in/FILE'`. A path in the Linux file system is
    refused with that advice. A Windows path given from bash goes in single quotes, or the shell eats
    its backslashes.
  - `pull GUESTPATH`: lands in the exchange's `lab-out\<time>-<random>\`, marked as from the internet;
    `lab-run` prints the new folder's WSL path. Read it there, or copy it into the project. A guest path
    holding links or junctions is refused: pull what they point to.
  - `sync REPO`: make `lab-in/REPO.bundle` first (`git -C prior-art/REPO bundle create
    /mnt/c/.../lab-in/REPO.bundle --all`, in the sandbox); `lab-run sync REPO` pushes it and clones it
    to the guest's `C:\work\REPO`. No git runs on the host for it.

## The lab (`lab/`, `cmd/lab-broker`, `cmd/lab-run`)

- **`lab-broker`** runs as SYSTEM (a scheduled task at startup, restarting on failure) and listens on
  `\\.\pipe\agent-containment-lab`.
  - The pipe gives SYSTEM, Administrators and the broker's own account full access, and the owner's
    account (every lab state's `client_sid`, which `install.ps1` sets) only read, write and synchronize. It does not get `FILE_CREATE_PIPE_INSTANCE`, so it can
    never add an instance of its own.
  - Remote clients are refused.
  - The broker creates its first instance with `FILE_FLAG_FIRST_PIPE_INSTANCE`, so a squatter on the
    name stops it from starting rather than posing as it. It then always holds a spare instance before
    handing a connection off.
  - `lab-run` refuses any server whose program is not `C:\Program Files\agent-containment\lab-broker.exe`
    (it checks the program's path, not the account it runs as: only administrators can write that
    folder).
  - Any caller the pipe admits may name any registered project and its VMs (`--project`, `--vm`). With
    one project and one owner that is the design; projects with different owners would need the
    broker to check which caller each project belongs to.
- **The host check, the lock** (`lab.HostCheck`): Hyper-V's own configuration against the lab state the
  owner's install recorded, which the guest cannot influence. It has no side effects, and it runs
  **before anything can start or reset the VM**, again after a start, and every minute during a run.
  It requires:
  - Phase A: no network adapter. A Phase B state (a Private switch) is refused until Phase B proves the
    switch's membership and its peers;
  - no DVD drive, no COM port on a pipe, no assigned (passed-through) device, no Fibre Channel adapter;
  - Guest Service Interface off, and the host's Enhanced Session mode off;
  - exactly the recorded disks, each over its recorded differencing parent;
  - the game disk's parent at its recorded size and write time;
  - at least 10 GiB free on every volume holding an attached disk's file. The differencing disks grow
    there, and a guest that rewrote its game volume once filled the host's C: in one boot (the VM
    paused, WSL crashed). A run's minute-by-minute recheck turns the VM off below it.
- **Between calls too** (`lab.Broker.Watch`): every minute the broker reads each registered lab VM
  that is running, and turns it off if its host check fails, ending any call on it, with a `watch`
  line in the log. It holds the VM as a call does, so it waits for a reset and a reset cancels it. A
  guest's own drive optimisation, started while no call ran, once wrote ~244 GB to the work disk.
- **The guest probe, a diagnostic** (`lab.Probe`), run only after the host check passes:
  - no adapter up;
  - no TCP connection to `1.1.1.1:443`, `8.8.8.8:53`, `192.0.2.1:80` or the host's own addresses;
  - no public DNS.
  - **Positive control in the same call:** a loopback connection succeeds, and the computer name is the
    recorded one.

  The guest's administrator could forge the probe, so it diagnoses rather than locks. Any failure of
  either is one `REFUSED:` line and exit 2. `status` never starts a VM. No flag, variable or file turns
  any of it off.
- **Transfers never let SYSTEM touch a host path on the caller's word.** Host paths are read and written
  as the caller (`ImpersonateNamedPipeClient`), so a planted junction leads only where the caller could
  go anyway. The privileged side works only in a fresh staging directory that only SYSTEM and
  Administrators can write.
  - `push` reads each object as the caller. Each must be inside the project's lab exchange (its
    registry `root`), and not a link or junction, descendants included. Because the sandbox can
    write `lab-in` while a push runs, each object is checked again once opened: the handle's final
    path, every link followed, must be inside the root's. A push stages at most 32 GiB, and less if
    the volume (which holds the VMs' disks) would keep under 20 GiB free.
  - `pull`: in the guest, a path holding a link or junction, or over the limit, is refused, and the
    rest is packed into one tar archive (`tar.exe`). The broker reads that archive over PowerShell
    Direct in 4 MiB chunks into a file it names itself, counting the bytes and stopping at the limit, and
    unpacks it in Go. Only plain files and directories are made. Links are left out. Absolute names,
    `..`, backslashes, colons, Windows device names (`CON`, `NUL`, `COM1`...), names ending in a dot
    or space, other entry kinds, and passing the bytes or 200,000 entries refuse the archive. Nothing a
    guest names reaches a file the broker writes as SYSTEM. Every file is then marked as from the
    internet and published as the caller into a new `lab-out\<time>-<random>\`. A failure publishes
    nothing. A pull lands on the host volume three times (archive, tree, lab-out), so its limit is a
    third of the free space over the 20 GiB reserve, at most 32 GiB.
  - Names that would mean something else on Windows (device names, a trailing dot or space, a colon
    naming a data stream) are refused in a push too, and a file is opened without following a link at
    its last name.
  - The host never mounts a disk the guest wrote to.
- **Deadlines:**
  - a request must arrive within 30 s, and each frame must be delivered within 60 s;
  - a client that goes away cancels its run;
  - calls share the VM: any number of `run`, `status`, `push` and `pull` at once (each has its own
    PowerShell Direct session and proves the gate itself); starting a stopped VM is one call at a
    time, so it starts once; a `reset` holds the VM alone, cancelling every call on it first, and new
    calls wait behind a waiting reset, at most 2 minutes;
  - a host check that fails during a run turns the VM off and ends every call on it;
  - a run's program runs in the guest in a child PowerShell that joins a Job Object of its own; when the
    run is cut short (its client goes away, its timeout, a reset), the broker terminates that job from
    a second PowerShell Direct session, so the program and everything it started stop, and only they:
    ending the host's side alone left a cancelled extraction running for 30 minutes (2026-11-21). A
    broker that is itself stopped (an install) cannot do this for the runs it was serving;
  - the gate gets 15 minutes, and a run its timeout (1 h by default, 24 h at most); the host check
    repeats every minute during a run;
  - push and pull have no time limit of their own; they are bounded by their byte limits;
  - a `reset` is not cut short by its client going away or another reset (a disk swapped half way would
    leave the VM unusable); it has its own 15 minutes, and makes each new child disk beside the old one
    before swapping it in. Each new child is granted to the VM's own account (`NT VIRTUAL MACHINE\<VM
    id>`) first: Hyper-V grants that only when it attaches a disk, and without it the VM cannot start.
- **Request data never becomes script text.** The broker's PowerShell scripts are fixed; a request reaches
  them only as base64 JSON in `LAB_INPUT`. The guest password is a DPAPI (LocalMachine) blob in
  `secrets\`, which only SYSTEM and Administrators can read. It is decrypted inside the SYSTEM process
  that uses it, and never printed, logged or passed on a command line.
- **The log** (`log\broker-<day>.log`, SYSTEM and Administrators only, because argv can hold anything
  the caller typed). One line goes before each call, a call whose line cannot be written is refused,
  and one line goes after it (a failure to write that one is not reported). It records the client's
  process id, the request (each string cut at 4 KiB, the cut marked) and the result, and unreadable
  requests too. `install.ps1` refuses to go on if `secrets\` can be read by anyone but SYSTEM and
  Administrators.
- **The VM** was built offline from the pinned Windows ISO (no Setup, no network) by scripts this
  repository no longer carries (they are in its history, before the WSL move). It is Gen 2 with Secure
  Boot and TPM, has no network adapter, and has a GPU partition. Its disks:
  - a base system disk under a differencing child that `reset` rebuilds;
  - a work disk that survives resets (`C:\work`);
  - the game disk, through a differencing child over a read-only parent (`C:\game`).

## Rebasing a lab's system disk (`install/rebase-os.ps1`)

The guest is Windows 11 Enterprise Evaluation. It must be activated online once, and the activation
lasts about 90 days; unactivated, or once it runs out, Windows shuts the guest down an hour after every
boot. A rebase gives the VM a fresh Evaluation system disk, activates it with nothing else attached,
and makes it the VM's sealed base. Repeat it every ~90 days. The owner runs the installed copy,
elevated (it asks once):

    powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Program Files\agent-containment\rebase-os.ps1" -VM game-lab

- **The rule, asserted at the start and end of every phase and around every adapter change:** the VM
  never has a network adapter while any disk but the new, blank system disk is attached. The game disk
  and the work disk (which may hold a quarantined sample) never meet a network. A breach turns the VM
  off and removes the adapter before the script stops.
- **Phases, resumable** (run the same line again and it continues where it stopped):
  1. **prepare**: the broker's task disabled, so no call can touch the VM meanwhile; the VM shut down;
     its disks checked against the lab state, recorded (path, controller, location) and detached; the
     old `os_base` and `os_diff` moved aside into `<vm folder>\rebase\`.
  2. **build**: a new system disk at the `os_base` path, with Windows applied offline from the ISO, as
     the lab was first built (DISM; no Windows Setup, no DVD drive). An answer file names the guest as
     the lab state does and creates `guest_user` with the password already in the credential blob, so
     `secrets\` does not change. The host's NVIDIA driver files go in for the GPU partition. The ISO is
     the lab's pinned copy in `media\` (named by its SHA-256), or `-Iso PATH -IsoSha256 HEX`, which is
     copied there and hashed; its `setup.exe` must carry Microsoft's signature.
  3. **activate**: the new disk alone in the VM. First boot, then a network adapter on Hyper-V's
     Default Switch, `slmgr /ato` until the license reads Licensed, and the adapter removed again
     (also on any failure). The license is read once more with no adapter. `guest_user`'s password is
     set never to expire (Windows' default is 42 days), and drive optimisation (the Defrag tasks
     and the `defragsvc` service) and NTFS last-access writes are turned off (they rewrote the game
     volume). The task alone did not stay off: something re-enabled it about 10 minutes after the first
     boot. So the guest is rebooted, and both must still be off. The activation's end date is printed.
  4. **finish**: the new disk made read-only (the new `os_base`), a new `os_diff` over it, and every
     recorded disk reattached where it was. Then the broker's task is enabled, and `lab-broker check`,
     `lab-run status` and a run through the gate (which starts the VM and probes it) prove the result.
- **The lab state file is never written:** the new disks take the old paths. (A PowerShell rewrite
  would also lose the game disk's recorded write time below the second, and the gate would refuse.)
- **It is built in the lab VM itself**, not in a separate one: activation is bound to the VM's
  virtual hardware, and a disk moved to another VM could need activating again, which it cannot do
  offline.
- **`-Rollback`** puts the old system disks back, deletes the new ones and reattaches the recorded
  disks, at any phase and after finishing, until **`-Forget`** deletes the old disks (it asks first). A
  new rebase needs `-Forget` first.
- **`-Seal`** folds `os_diff` into `os_base`, so a reset returns to the guest as it is now. After a
  rebase the guest has nothing installed: set it up (tools, mount points), then seal. Only seal a guest
  that has run nothing untrusted since its last reset, because what is in `os_diff` becomes part of
  every reset.
- The host mounts only the disk it is building and the ISO, never a disk the guest wrote to.

## Install and uninstall

- **Windows** (`install/install.ps1`, elevated, one command): clones main fresh into
  `C:\ProgramData\agent-containment\src\<time>` (admin-only) with the Git and Go in `C:\Program Files`
  and none of the profile's configuration; vets, runs the suite and builds there; installs
  `lab-broker.exe`, `lab-run.exe` and `rebase-os.ps1` into `C:\Program Files\agent-containment\`, the registry into
  `C:\ProgramData\agent-containment\projects\`, and each lab exchange; sets every lab state's client to
  the owner (`lab-broker set-owner`); turns the host's Enhanced Session mode off; registers the one
  task (`\agent-containment\lab-broker`, SYSTEM, at startup); and ends with `lab-broker check` and
  `lab-run status` against each real VM. `install/uninstall.ps1` removes exactly that and keeps the VMs,
  their disks, the lab states, secrets and the broker's log.
- **WSL** (`wsl/install.sh`, as root): clones main (or `BRANCH`) fresh into a root-only directory and
  refuses to go on unless it is that clone's own `install.sh`; tests and builds `edit-hook` and the
  static `lab-run` there; installs them and `projects.json` (only once `lab-run.exe` exists for the
  wrapper); and writes `/etc/claude-code/managed-settings.json`. `wsl/uninstall.sh` removes it.

Install Windows first, then WSL (the wrapper is made only when `lab-run.exe` is there).
