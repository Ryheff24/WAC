package main

import (
	"path/filepath"
	"strings"
)

// HookPath and LabRunPath are where install.sh puts the hook and the lab-run link (root-owned).
const (
	HookPath   = "/usr/local/lib/agent-containment/edit-hook"
	LabRunPath = "/usr/local/bin/lab-run"
)

// AllowedDomains are the only hosts a sandboxed command reaches: GitHub, for make pull and git.
var AllowedDomains = []string{"github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"}

// CredentialPaths are the places in a home that hold credentials, which no sandboxed command reads
// (with the network open to GitHub, anything read could be uploaded).
var CredentialPaths = []string{
	".ssh", ".gnupg", ".claude/.credentials.json", ".codex/auth.json", ".config/gh", ".git-credentials",
	".netrc", ".aws", ".azure", ".config/gcloud", ".kube", ".docker/config.json",
	".claude.json", ".config/git/credentials", ".npmrc", ".pypirc", ".cargo/credentials.toml",
}

// homeOf is the home directory holding root (/home/NAME/...), or "".
func homeOf(root string) string {
	parts := strings.Split(filepath.Clean(root), "/")
	if len(parts) >= 3 && parts[0] == "" && parts[1] == "home" && parts[2] != "" {
		return "/home/" + parts[2]
	}
	return ""
}

// Settings is /etc/claude-code/managed-settings.json. Managed settings outrank every user and project
// file, so nothing an agent can write loosens them.
//
//   - Every shell command runs in the sandbox (bubblewrap). If the sandbox cannot start, Claude Code
//     refuses to run commands. A request to run one outside it is ignored.
//   - The one command outside the sandbox is lab-run, spelled by its root-owned path: it is the way
//     into the lab VM, where untrusted code runs. Every part of a compound command must match it, or
//     the whole call stays sandboxed.
//   - Sandboxed commands cannot write the projects' protected paths, and reach only AllowedDomains.
//     Outside the project they may write each project's lab-in on Windows (lab_in), the folder
//     lab-run push reads from; nothing there runs on the host, and nothing in it is protected.
//   - They read nothing on the Windows drives (/mnt, whole: a path denied under /mnt/c could be
//     reached by another spelling, case or 8.3, of its name) but each project's lab exchange (the
//     folder holding lab_in), and none of the home's credentials (CredentialPaths). Only these
//     managed settings can re-open a read (allowManagedReadPathsOnly).
//   - The file tools, which the sandbox does not cover, go through edit-hook. Only managed hooks run.
func Settings(config Config) map[string]any {
	denyWrite, allowWrite, allowRead := []string{}, []string{}, []string{}
	denyRead := []string{"/mnt"}
	homes := map[string]bool{}
	for _, project := range config.Projects {
		for _, name := range project.Protected {
			denyWrite = append(denyWrite, filepath.Join(project.Root, name))
		}
		if project.LabIn != "" {
			allowWrite = append(allowWrite, project.LabIn)
			allowRead = append(allowRead, filepath.Dir(project.LabIn))
		}
		if home := homeOf(project.Root); home != "" && !homes[home] {
			homes[home] = true
			for _, name := range CredentialPaths {
				denyRead = append(denyRead, filepath.Join(home, name))
			}
		}
	}
	return map[string]any{
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"allowUnsandboxedCommands": false,
			"autoAllowBashIfSandboxed": true,
			"excludedCommands":         []string{LabRunPath + " *"},
			"filesystem": map[string]any{
				"denyWrite": denyWrite, "allowWrite": allowWrite,
				"denyRead": denyRead, "allowRead": allowRead, "allowManagedReadPathsOnly": true,
			},
			"network": map[string]any{
				"allowManagedDomainsOnly": true,
				"allowedDomains":          AllowedDomains,
			},
		},
		"allowManagedHooksOnly": true,
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "Edit|Write|MultiEdit|NotebookEdit",
				"hooks":   []any{map[string]any{"type": "command", "command": HookPath, "timeout": 10}},
			}},
		},
		// lab-run is approved outright: it only reaches the lab VM, whose broker checks the VM has
		// no network before every call.
		"permissions": map[string]any{
			"disableBypassPermissionsMode": "disable",
			"allow":                        []string{"Bash(" + LabRunPath + " *)"},
		},
	}
}
