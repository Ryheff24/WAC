#!/bin/bash
# Installs the WSL containment. Run it as root:
#   sudo bash wsl/install.sh        (or, from Windows: wsl -u root -- bash wsl/install.sh)
# BRANCH=NAME installs another branch than main (to try a pull request before it is merged).
#
#   1. clones BRANCH (main) of the repository fresh into a root-only build directory, refuses to go on
#      unless this script is that clone's own wsl/install.sh, and runs the rest from the clone's copy
#      (exec), so nothing more is read from the copy it was started from: then the tests and builds of
#      edit-hook and lab-run, there. Only the lines up to the clone run from the copy it was started
#      from; for those to be the owner's, start it from a copy no agent can write.
#   2. installs, root-owned: /usr/local/lib/agent-containment/{edit-hook,projects.json}
#   3. /usr/local/bin/lab-run (labrun/, a static binary): runs the Windows lab-run.exe, if it is installed
#   4. writes /etc/claude-code/managed-settings.json from projects.json (the earlier file is kept
#      beside it), then checks every line
# Running it again is harmless. uninstall.sh removes all of it. ROOT is for the tests only: it installs
# under ROOT from this script's own directory, without a clone.
set -euo pipefail
ROOT=${ROOT:-}
BRANCH=${BRANCH:-main}
repository=https://github.com/Ryheff24/WAC.git
here=$(cd "$(dirname "$0")" && pwd)
lib=$ROOT/usr/local/lib/agent-containment
settings_dir=$ROOT/etc/claude-code
labrun=$ROOT/usr/local/bin/lab-run
windows_labrun="/mnt/c/Program Files/agent-containment/lab-run.exe"
# Tests (ROOT set) may stand in another program, so the lab-run step runs without Windows.
if [ -n "$ROOT" ] && [ -n "${WINDOWS_LABRUN:-}" ]; then windows_labrun=$WINDOWS_LABRUN; fi

if [ -z "$ROOT" ] && [ "$(id -u)" != 0 ]; then echo "install.sh: run it as root (sudo bash $0)"; exit 1; fi

if [ -n "$ROOT" ]; then
  build=$(mktemp -d)
  trap 'rm -rf "$build"' EXIT
  echo "== tests and build (ROOT set: this directory, no clone)"
  cp -r "$here/." "$build/clone-wsl"
elif [ -n "${WAC_CLONE:-}" ]; then
  # The clone's own copy, run by the copy below: everything from here on is the clone's.
  build=$WAC_CLONE
  trap 'rm -rf "$build"' EXIT
  [ "$here" = "$build/clone/wsl" ] || { echo "install.sh: WAC_CLONE is set but this is not the clone's copy"; exit 1; }
  echo "$BRANCH at $(git -C "$build/clone" rev-parse HEAD)"
  cp -r "$build/clone/wsl" "$build/clone-wsl"
else
  build=$(mktemp -d)
  echo "== a fresh clone of $BRANCH (root-only directory)"
  # The repository is private, and root in WSL has no GitHub login: the clone asks Git for Windows'
  # credential manager, which holds the owner's (run through WSL interop, as the owner). Only this one
  # command gets a credential helper; no global or system git configuration is read.
  helper=()
  gcm="/mnt/c/Program Files/Git/mingw64/bin/git-credential-manager.exe"
  if [ -x "$gcm" ]; then helper=(-c credential.helper= -c "credential.helper=${gcm// /\\ }"); fi
  GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 \
    git "${helper[@]}" clone --quiet --depth 1 --branch "$BRANCH" "$repository" "$build/clone" || { rm -rf "$build"; exit 1; }
  if ! cmp -s "$0" "$build/clone/wsl/install.sh"; then
    echo "install.sh: this script is not $BRANCH's wsl/install.sh: run that branch's own copy, or set BRANCH to the branch this one came from (nothing was installed)"
    rm -rf "$build"
    exit 1
  fi
  exec env WAC_CLONE="$build" BRANCH="$BRANCH" bash "$build/clone/wsl/install.sh"
fi
source=$build/clone-wsl
echo "== tests and build"
cp -r "$source/." "$build/src"
(
  cd "$build/src"
  export GOCACHE="$build/gocache" GOPATH="$build/gopath" GOFLAGS=-buildvcs=false GOTOOLCHAIN=local GOPROXY=off
  go vet ./...
  go test -count=1 ./...
  go build -o "$build/edit-hook" ./edithook
  CGO_ENABLED=0 go build -o "$build/lab-run" ./labrun
)

echo "== install"
install -d -m 0755 "$lib" "$settings_dir" "$(dirname "$labrun")"
install -m 0755 "$build/edit-hook" "$lib/edit-hook"
install -m 0644 "$source/projects.json" "$lib/projects.json"
if [ -x "$windows_labrun" ]; then
  install -m 0755 "$build/lab-run" "$labrun"
  echo "lab-run: $labrun runs $windows_labrun"
else
  rm -f "$labrun"
  echo "lab-run: $windows_labrun is not installed yet (the Windows side); $labrun not made"
fi
"$lib/edit-hook" settings "$lib/projects.json" > "$build/managed-settings.json"
if [ -f "$settings_dir/managed-settings.json" ] && ! cmp -s "$settings_dir/managed-settings.json" "$build/managed-settings.json"; then
  cp -p "$settings_dir/managed-settings.json" "$settings_dir/managed-settings.json.before-$(date +%Y%m%d%H%M%S)"
fi
install -m 0644 "$build/managed-settings.json" "$settings_dir/managed-settings.json"
if [ -z "$ROOT" ]; then chown -R root:root "$lib" "$settings_dir/managed-settings.json"; fi

echo "== check (every line must say OK)"
failed=0
check() { if eval "$2"; then echo "OK   $1"; else echo "FAIL $1"; failed=1; fi; }
owner() { [ -n "$ROOT" ] || [ "$(stat -c %U "$1")" = root ]; }
for file in "$lib/edit-hook" "$lib/projects.json" "$settings_dir/managed-settings.json"; do
  check "$file is root-owned and writable by nobody else" "owner '$file' && [ \$(( 0\$(stat -c %a '$file') & 022 )) = 0 ]"
done
check "managed settings match projects.json" "cmp -s '$settings_dir/managed-settings.json' <('$lib/edit-hook' settings '$lib/projects.json')"
check "the hook refuses an edit of a protected file" "! printf '%s' '{\"tool_name\":\"Write\",\"cwd\":\"/\",\"tool_input\":{\"file_path\":\"/home/user/projects/game/Makefile\"}}' | CLAUDE_PROJECT_DIR=/home/user/projects/game '$lib/edit-hook' 2>/dev/null"
if [ -z "$ROOT" ]; then
  # The control: an edit the hook must allow. Without it, a hook that refuses everything passes above.
  check "the hook allows an edit inside the project (control)" "printf '%s' '{\"tool_name\":\"Write\",\"cwd\":\"/\",\"tool_input\":{\"file_path\":\"/home/user/projects/game/notes/x.md\"}}' | CLAUDE_PROJECT_DIR=/home/user/projects/game '$lib/edit-hook'"
fi
if [ -e "$labrun" ]; then
  check "$labrun is the one just built, static, root-owned and writable by nobody else" "cmp -s '$labrun' '$build/lab-run' && ! grep -q ld-linux '$labrun' && owner '$labrun' && [ \$(( 0\$(stat -c %a '$labrun') & 022 )) = 0 ]"
fi
check "bubblewrap and socat are installed (the sandbox needs both)" "command -v bwrap >/dev/null && command -v socat >/dev/null"
[ "$failed" = 0 ] || { echo "install.sh: a check failed"; exit 2; }
echo "Installed. New Claude Code sessions (and claude -c) start with it."
