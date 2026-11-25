#!/bin/bash
# Removes what install.sh installed: the managed settings first (Claude Code reloads them, so a
# running session is not left calling a missing hook), then the hook, its config and lab-run.
#   sudo bash wsl/uninstall.sh      (or, from Windows: wsl -u root -- bash wsl/uninstall.sh)
# Backups of earlier managed settings (managed-settings.json.before-*) are kept. ROOT is for tests.
set -euo pipefail
ROOT=${ROOT:-}
if [ -z "$ROOT" ] && [ "$(id -u)" != 0 ]; then echo "uninstall.sh: run it as root (sudo bash $0)"; exit 1; fi
rm -f "$ROOT/etc/claude-code/managed-settings.json"
rm -rf "$ROOT/usr/local/lib/agent-containment"
rm -f "$ROOT/usr/local/bin/lab-run"
left=0
for path in "$ROOT/etc/claude-code/managed-settings.json" "$ROOT/usr/local/lib/agent-containment" "$ROOT/usr/local/bin/lab-run"; do
  if [ -e "$path" ]; then echo "STILL THERE: $path"; left=1; fi
done
[ "$left" = 0 ] && echo "Uninstalled. Restart Claude Code sessions to drop the sandbox settings."
exit "$left"
