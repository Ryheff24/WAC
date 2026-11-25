# WAC

The agent containment for every project on this PC: a game project (`game`) first, d1preservation (d1pres) later. Agents
(Claude Code) run in WSL2 inside Claude Code's own sandbox; untrusted code runs only in a Hyper-V lab
VM with no network adapter, reached through one command, `lab-run`. It keeps d1pres `containment/`'s
core: **Go outside the game process, C inside it**, so the owner can read the boundary they are
trusting.

Read in this order:

| file | what |
|---|---|
| [`docs/agent-containment.md`](docs/agent-containment.md) | The rule, the two places (the WSL sandbox, the lab VM), the way between them, the lab's gate |
| [`docs/decisions.md`](docs/decisions.md) | Decisions and their reasons, dated |
| `wsl/` | The WSL side: `install.sh` / `uninstall.sh`, the managed settings and `edit-hook` (Go), `projects.json`, and `labrun/` (`/usr/local/bin/lab-run`, a static Go binary) |
| `lab/` | The lab: the request protocol, the gate proved on every call, the broker's verbs and transfers, the named pipe, and Hyper-V through fixed PowerShell scripts |
| `cmd/lab-broker/`, `cmd/lab-run/` | The SYSTEM broker and its client (`lab-run -- PROGRAM`, `status`, `push`, `pull`, `sync`, `reset`) |
| `registry/` | The Windows project registry: each project's lab VMs, its lab exchange on Windows, and its WSL root |
| `install/` | The Windows side: `install.ps1` / `uninstall.ps1` |
| `.github/workflows/coverage.yml` | CI on every PR and on main: gofmt, vet, both install scripts parse, the suite with coverage on Windows (the built binaries' runs included), the coverage gate (60% total, no file at 0%), and the WSL module's tests and install on Linux |

## Install

1. **Windows**, in an elevated PowerShell, from a clone of this repository (it builds from a fresh clone
   of main, not from yours):

   ```
   powershell -NoProfile -ExecutionPolicy Bypass -File install\install.ps1
   ```

   It ends with `lab-run status` against the real lab VM. `install\uninstall.ps1` undoes it and keeps
   the VM.
2. **WSL**, from a fresh clone: `sudo bash wsl/install.sh`. `sudo bash wsl/uninstall.sh` undoes it.

Not built yet: `gate/`, the C game gate compiled into every game and test binary (when there is a game
binary), Phase B's server VM on a Private switch, and the Codex layer for WSL
(`/etc/codex/requirements.toml`).
