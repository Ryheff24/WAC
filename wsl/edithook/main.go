// edit-hook is a Claude Code PreToolUse hook for the file tools the sandbox does not cover (Edit,
// Write, MultiEdit, NotebookEdit). Shell commands are the sandbox's: bubblewrap, set up by
// /etc/claude-code/managed-settings.json.
//
// In every session, wherever it started, no file in a configured project may be written if it is in
// a protected path (Makefile, scripts, ...), in an untrusted tree (prior-art, downloads, ...), or
// anything at any depth named .git, .claude, .mcp.json, CLAUDE.md, CLAUDE.local.md or AGENTS.md.
//
// In a session whose project is in the config, a file may moreover be written only:
//   - inside the project;
//   - in the session's scratchpad (/tmp/claude-UID);
//   - in the project's lab-in on Windows (lab_in), as the sandbox allows commands;
//   - as a Markdown file in the project's memory folder or in ~/.claude/plans.
//
// Paths are compared after following links, so a link cannot point an edit elsewhere. A session
// outside every configured project may write anything else. An event or config it cannot read is
// refused (fail closed).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ConfigPath is the root-owned project list install.sh writes. Only tests build it with another
// (-ldflags "-X main.ConfigPath=...").
var ConfigPath = "/usr/local/lib/agent-containment/projects.json"

type Project struct {
	Name      string   `json:"name"`
	Root      string   `json:"root"`
	Protected []string `json:"protected"`
	Untrusted []string `json:"untrusted"`
	// LabIn is the project's lab-in on Windows (/mnt/c/...): the one place outside the project the
	// sandbox lets commands write, so a file can be put there for lab-run push.
	LabIn string `json:"lab_in,omitempty"`
}

type Config struct {
	Projects []Project `json:"projects"`
}

type event struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// Env is what the rules need from the account: its home and the session scratch root.
type Env struct {
	Home    string
	Scratch string
}

var editTools = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

// neverNames may not be written anywhere in a project, at any depth: Claude Code's and git's
// configuration, and the instruction files Claude Code and Codex load (a nested CLAUDE.md is loaded
// too).
var neverNames = []string{".git", ".claude", ".mcp.json", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "settings" {
		// edit-hook settings CONFIG: the managed settings for that project list (install.sh).
		config, err := Load(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		text, err := json.MarshalIndent(Settings(config), "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(string(text))
		return
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		refuse("cannot read the event: " + err.Error())
	}
	config, err := Load(ConfigPath)
	if err != nil {
		refuse(err.Error())
	}
	home, _ := os.UserHomeDir()
	env := Env{Home: home, Scratch: fmt.Sprintf("/tmp/claude-%d", os.Getuid())}
	if why := Decide(raw, os.Getenv("CLAUDE_PROJECT_DIR"), config, env); why != "" {
		refuse(why)
	}
}

func refuse(why string) {
	fmt.Fprintln(os.Stderr, "edit-hook: BLOCKED - "+why)
	os.Exit(2)
}

// Load reads the project list.
func Load(path string) (Config, error) {
	var config Config
	data, err := os.ReadFile(path)
	if err != nil {
		return config, fmt.Errorf("cannot read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("cannot parse %s: %v", path, err)
	}
	for _, project := range config.Projects {
		if !filepath.IsAbs(project.Root) {
			return config, fmt.Errorf("%s: project %q has no absolute root", path, project.Name)
		}
		if project.LabIn != "" && (!strings.HasPrefix(project.LabIn, "/mnt/") || filepath.Clean(project.LabIn) != project.LabIn || strings.Count(project.LabIn, "/") < 4) {
			return config, fmt.Errorf("%s: project %q: lab_in %q is not a clean folder on a Windows drive (/mnt/c/...)", path, project.Name, project.LabIn)
		}
	}
	return config, nil
}

// Decide returns why the event is refused, or "".
func Decide(raw []byte, projectDir string, config Config, env Env) string {
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return "the event is not one this hook can read (" + err.Error() + ")"
	}
	if !slices.Contains(editTools, ev.ToolName) {
		return ""
	}
	project := sessionProject(config, projectDir, ev.Cwd)
	target := ev.ToolInput.FilePath
	if ev.ToolName == "NotebookEdit" && ev.ToolInput.NotebookPath != "" {
		target = ev.ToolInput.NotebookPath
	}
	if target == "" {
		if project == nil {
			return ""
		}
		return ev.ToolName + ": no file path in the event"
	}
	if !filepath.IsAbs(target) {
		if !filepath.IsAbs(ev.Cwd) {
			if project == nil {
				return ""
			}
			return ev.ToolName + ": " + target + " is relative and the session's directory is unknown; give the path absolute"
		}
		target = filepath.Join(ev.Cwd, target)
	}
	real, err := RealPath(target)
	if err != nil {
		return ev.ToolName + ": " + err.Error()
	}
	refused := ev.ToolName + " of " + target + " is refused"
	if real != filepath.Clean(target) {
		refused += " (it is " + real + ")"
	}

	// A registered project's own rules hold for every session, wherever it was started: the file
	// tools run outside the sandbox, so a session in another directory could otherwise write them.
	for _, owner := range config.Projects {
		if why := projectRules(owner, real); why != "" {
			return refused + ": " + why
		}
	}
	if project == nil {
		return ""
	}
	root, err := RealPath(project.Root)
	if err != nil {
		return "project " + project.Name + ": " + err.Error()
	}
	if Within(root, real) {
		return ""
	}
	if scratch, err := RealPath(env.Scratch); err == nil && Within(scratch, real) {
		return ""
	}
	// The project's lab-in, which the sandbox lets commands write too (lab_in): what goes there is
	// pushed into the lab. Compared after following links, so a link in lab-in leads nowhere else.
	if project.LabIn != "" {
		if labIn, err := RealPath(project.LabIn); err == nil && Within(labIn, real) {
			return ""
		}
	}
	if strings.HasSuffix(strings.ToLower(real), ".md") {
		memory := filepath.Join(env.Home, ".claude", "projects", ProjectKey(project.Root), "memory")
		plans := filepath.Join(env.Home, ".claude", "plans")
		for _, place := range []string{memory, plans} {
			if resolved, err := RealPath(place); err == nil && Within(resolved, real) {
				return ""
			}
		}
	}
	return refused + ": in project " + project.Name + " files are written only inside the project, the session's scratchpad, and Markdown in the project's memory or ~/.claude/plans"
}

// projectRules is why project's rules refuse writing real (a resolved path), or "".
func projectRules(project Project, real string) string {
	root, err := RealPath(project.Root)
	if err != nil || !Within(root, real) {
		return ""
	}
	relative, _ := filepath.Rel(root, real)
	for _, part := range strings.Split(relative, "/") {
		if slices.Contains(neverNames, part) {
			return part + " is Claude Code's, Codex's or git's configuration or instructions, which no agent writes"
		}
	}
	for _, name := range project.Protected {
		if Within(filepath.Join(root, name), real) {
			return name + " is protected in project " + project.Name + "; the owner changes it"
		}
	}
	for _, name := range project.Untrusted {
		if Within(filepath.Join(root, name), real) {
			return name + " holds third-party data, which agents read but never change"
		}
	}
	return ""
}

// sessionProject is the configured project holding the session: CLAUDE_PROJECT_DIR first, then cwd.
func sessionProject(config Config, projectDir, cwd string) *Project {
	for _, directory := range []string{projectDir, cwd} {
		if !filepath.IsAbs(directory) {
			continue
		}
		real, err := RealPath(directory)
		if err != nil {
			continue
		}
		for i, project := range config.Projects {
			if root, err := RealPath(project.Root); err == nil && Within(root, real) {
				return &config.Projects[i]
			}
		}
	}
	return nil
}

// RealPath is path with every link followed: the longest part that exists is resolved, and the
// rest (not yet created) appended. A link whose target does not exist yet is followed to that target
// (a write through it creates the target): judged by its own name, notes/x.md -> CLAUDE.md passed
// (review 2026-11-21).
func RealPath(path string) (string, error) {
	return realPath(path, 0)
}

func realPath(path string, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("%s: too many links", path)
	}
	path = filepath.Clean(path)
	existing, rest := path, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s cannot be resolved: %v", path, err)
		}
		if info, lerr := os.Lstat(existing); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			target, rerr := os.Readlink(existing)
			if rerr != nil {
				return "", fmt.Errorf("%s cannot be resolved: %v", path, rerr)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(existing), target)
			}
			return realPath(filepath.Join(target, rest), depth+1)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("%s cannot be resolved", path)
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

// Within reports whether path is directory or inside it. Under /mnt (Windows drives, where names
// ignore case) the comparison ignores case too.
func Within(directory, path string) bool {
	if strings.HasPrefix(directory, "/mnt/") || strings.HasPrefix(path, "/mnt/") {
		directory, path = strings.ToLower(directory), strings.ToLower(path)
	}
	return path == directory || strings.HasPrefix(path, strings.TrimSuffix(directory, "/")+"/")
}

// ProjectKey is the folder name Claude Code gives a project under ~/.claude/projects.
func ProjectKey(root string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, root)
}
