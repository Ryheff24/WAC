package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// setup makes a project, a scratch root, a home with a memory folder and plans, and links that
// point out of the project.
func setup(t *testing.T) (Config, Env, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "projects", "game")
	home := filepath.Join(base, "home")
	scratch := filepath.Join(base, "scratch")
	for _, directory := range []string{
		filepath.Join(root, "prior-art", "Upstream"), filepath.Join(root, "scripts"), filepath.Join(root, ".claude"),
		filepath.Join(root, "notes"), filepath.Join(home, ".claude", "projects", ProjectKey(root), "memory"),
		filepath.Join(home, ".claude", "plans"), scratch,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{filepath.Join(root, "Makefile"), filepath.Join(home, ".bashrc")} {
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Links an agent's sandboxed shell could make inside the project.
	links := map[string]string{
		filepath.Join(root, "notes", "bashrc-link"): filepath.Join(home, ".bashrc"),
		filepath.Join(root, "notes", "make-link"):   filepath.Join(root, "Makefile"),
		filepath.Join(root, "notes", "out"):         home,
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{Projects: []Project{{
		Name: "game", Root: root,
		Protected: []string{"Makefile", "CLAUDE.md", "scripts", ".claude", ".mcp.json"},
		Untrusted: []string{"prior-art", "downloads"},
	}}}
	return config, Env{Home: home, Scratch: scratch}, root, home
}

func eventJSON(t *testing.T, tool, path, cwd string) []byte {
	t.Helper()
	input := map[string]string{"file_path": path}
	if tool == "NotebookEdit" {
		input = map[string]string{"notebook_path": path}
	}
	raw, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "cwd": cwd, "tool_input": input})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecide(t *testing.T) {
	config, env, root, home := setup(t)
	memory := filepath.Join(home, ".claude", "projects", ProjectKey(root), "memory")
	cases := []struct {
		name  string
		tool  string
		path  string
		allow bool
	}{
		{"a new file in the project", "Write", filepath.Join(root, "notes", "plan.md"), true},
		{"a relative path in the project", "Edit", "notes/plan.md", true},
		{"a notebook in the project", "NotebookEdit", filepath.Join(root, "notes", "a.ipynb"), true},
		{"the scratchpad", "Write", filepath.Join(env.Scratch, "x", "probe.sh"), true},
		{"memory, Markdown", "Write", filepath.Join(memory, "note.md"), true},
		{"plans, Markdown", "Write", filepath.Join(home, ".claude", "plans", "p.md"), true},
		{"memory, not Markdown", "Write", filepath.Join(memory, "x.sh"), false},
		{"a protected file", "Edit", filepath.Join(root, "Makefile"), false},
		{"a protected file, relative", "Write", "CLAUDE.md", false},
		{"inside a protected tree", "Write", filepath.Join(root, "scripts", "lab.sh"), false},
		{".claude settings", "Write", filepath.Join(root, ".claude", "settings.local.json"), false},
		{".mcp.json", "Write", filepath.Join(root, ".mcp.json"), false},
		{".git anywhere in the project", "Write", filepath.Join(root, "notes", ".git", "config"), false},
		{".claude anywhere in the project", "Write", filepath.Join(root, "notes", ".claude", "settings.json"), false},
		{"an untrusted tree", "Edit", filepath.Join(root, "prior-art", "Upstream", "run.py"), false},
		{"outside the project", "Write", filepath.Join(home, ".bashrc"), false},
		{"outside, by ..", "Write", filepath.Join(root, "..", "..", "home", ".bashrc"), false},
		{"a link to a file outside", "Edit", filepath.Join(root, "notes", "bashrc-link"), false},
		{"a link to a protected file", "Write", filepath.Join(root, "notes", "make-link"), false},
		{"through a linked directory", "Write", filepath.Join(root, "notes", "out", ".profile"), false},
		{"another tool passes", "Read", filepath.Join(home, ".bashrc"), true},
	}
	for _, c := range cases {
		why := Decide(eventJSON(t, c.tool, c.path, root), root, config, env)
		if c.allow && why != "" {
			t.Errorf("%s: refused: %s", c.name, why)
		}
		if !c.allow && why == "" {
			t.Errorf("%s: allowed", c.name)
		}
	}
}

func TestOutsideEveryProject(t *testing.T) {
	config, env, _, home := setup(t)
	elsewhere := filepath.Join(home, "other")
	if why := Decide(eventJSON(t, "Write", filepath.Join(home, ".bashrc"), elsewhere), elsewhere, config, env); why != "" {
		t.Errorf("a session outside every project was refused: %s", why)
	}
}

// TestDanglingLinks: a link to a file that does not exist yet is judged by its target, from the
// project's session and from outside; a dangling link to an ordinary place still passes (the control).
func TestDanglingLinks(t *testing.T) {
	config, env, root, home := setup(t)
	elsewhere := filepath.Join(home, "other")
	os.MkdirAll(elsewhere, 0o755)
	links := map[string]string{
		filepath.Join(root, "notes", "to-claude.md"): filepath.Join(root, "notes", "deep", "CLAUDE.md"),
		filepath.Join(root, "notes", "relative.md"):  "../scripts/new.sh",
		filepath.Join(elsewhere, "to-makefile"):      filepath.Join(root, "Makefile.new"),
		filepath.Join(elsewhere, "chain"):            filepath.Join(elsewhere, "to-claude-2"),
		filepath.Join(elsewhere, "to-claude-2"):      filepath.Join(root, "CLAUDE.md"),
		filepath.Join(root, "notes", "fine.md"):      filepath.Join(root, "notes", "new.md"),
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	config.Projects[0].Protected = append(config.Projects[0].Protected, "Makefile.new")
	for _, c := range []struct {
		path, session string
		allow         bool
	}{
		{filepath.Join(root, "notes", "to-claude.md"), root, false},
		{filepath.Join(root, "notes", "relative.md"), root, false},
		{filepath.Join(elsewhere, "to-makefile"), elsewhere, false},
		{filepath.Join(elsewhere, "chain"), elsewhere, false},
		{filepath.Join(root, "notes", "fine.md"), root, true},
	} {
		why := Decide(eventJSON(t, "Write", c.path, c.session), c.session, config, env)
		if (why == "") != c.allow {
			t.Errorf("%s from %s: %q (allow %v)", c.path, c.session, why, c.allow)
		}
	}
}

// TestLabIn: the project's own session may write its lab-in (as the sandbox lets commands), but not
// through a link out of it, nor the lab exchange around it, nor another session's (controls).
func TestLabIn(t *testing.T) {
	config, env, root, home := setup(t)
	labIn := filepath.Join(home, "agent-lab", "game", "lab-in")
	os.MkdirAll(labIn, 0o755)
	config.Projects[0].LabIn = labIn
	os.Symlink(filepath.Join(home, ".bashrc"), filepath.Join(labIn, "out"))
	elsewhere := filepath.Join(home, "other")
	for _, c := range []struct {
		path, session string
		allow         bool
	}{
		{filepath.Join(labIn, "build.ps1"), root, true},
		{filepath.Join(labIn, "sub", "x.bin"), root, true},
		{filepath.Join(labIn, "out"), root, false},
		{filepath.Join(filepath.Dir(labIn), "lab-out", "x"), root, false},
		{filepath.Join(labIn, "build.ps1"), elsewhere, true},
	} {
		why := Decide(eventJSON(t, "Write", c.path, c.session), c.session, config, env)
		if (why == "") != c.allow {
			t.Errorf("%s from %s: %q (allow %v)", c.path, c.session, why, c.allow)
		}
	}
}

// TestProjectRulesHoldEverywhere: a session started outside every project (this repository, ~) still
// cannot write a project's protected paths, untrusted trees or instruction files, at any depth, nor
// through a link; it can write the project's ordinary files and anything else (the controls).
func TestProjectRulesHoldEverywhere(t *testing.T) {
	config, env, root, home := setup(t)
	elsewhere := filepath.Join(home, "other")
	for _, path := range []string{
		filepath.Join(root, "Makefile"), filepath.Join(root, "CLAUDE.md"), filepath.Join(root, "scripts", "x.sh"),
		filepath.Join(root, ".claude", "settings.json"), filepath.Join(root, "prior-art", "Upstream", "a.c"),
		filepath.Join(root, "notes", "deep", "CLAUDE.md"), filepath.Join(root, "CLAUDE.local.md"), filepath.Join(root, "notes", "AGENTS.md"),
		filepath.Join(root, ".git", "config"), filepath.Join(root, "notes", "make-link"),
	} {
		for _, projectDir := range []string{elsewhere, home, ""} {
			if why := Decide(eventJSON(t, "Write", path, elsewhere), projectDir, config, env); why == "" {
				t.Errorf("a session in %q wrote %s", projectDir, path)
			}
		}
	}
	for _, path := range []string{filepath.Join(root, "notes", "x.md"), filepath.Join(home, ".bashrc"), filepath.Join(elsewhere, "CLAUDE.md")} {
		if why := Decide(eventJSON(t, "Write", path, elsewhere), elsewhere, config, env); why != "" {
			t.Errorf("a session outside every project was refused %s: %s", path, why)
		}
	}
	// In the project's own session, nested instruction files are refused too; an ordinary file is not.
	for path, allow := range map[string]bool{filepath.Join(root, "notes", "CLAUDE.md"): false, filepath.Join(root, "notes", "CLAUDE.local.md"): false,
		filepath.Join(root, "notes", "AGENTS.md"): false, filepath.Join(root, "notes", "CLAUDE.md.txt"): true} {
		if why := Decide(eventJSON(t, "Write", path, root), root, config, env); (why == "") != allow {
			t.Errorf("in the project's session, %s: %q (allow %v)", path, why, allow)
		}
	}
}

func TestProjectFromCwd(t *testing.T) {
	config, env, root, home := setup(t)
	// No CLAUDE_PROJECT_DIR: the cwd decides.
	if why := Decide(eventJSON(t, "Write", filepath.Join(home, ".bashrc"), filepath.Join(root, "notes")), "", config, env); why == "" {
		t.Error("a session in the project (by cwd) wrote outside it")
	}
}

func TestFailsClosed(t *testing.T) {
	config, env, root, _ := setup(t)
	for name, raw := range map[string]string{
		"not JSON":         "{",
		"no path":          `{"tool_name":"Write","cwd":"` + root + `","tool_input":{}}`,
		"wrong path type":  `{"tool_name":"Write","cwd":"` + root + `","tool_input":{"file_path":3}}`,
		"relative, no cwd": `{"tool_name":"Write","tool_input":{"file_path":"x.md"}}`,
	} {
		if why := Decide([]byte(raw), root, config, env); why == "" {
			t.Errorf("%s: allowed", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a missing config loaded")
	}
}

func TestWithinIgnoresCaseOnWindowsDrives(t *testing.T) {
	if !Within("/mnt/c/Users/x/game", "/mnt/c/users/X/GAME/Makefile") {
		t.Error("a /mnt path in another case was not inside")
	}
	if Within("/home/x/game", "/home/x/GAME/Makefile") {
		t.Error("an ext4 path in another case counted as inside")
	}
	if Within("/home/x/game", "/home/x/game-other/f") {
		t.Error("a sibling with the same prefix counted as inside")
	}
}

func TestProjectKey(t *testing.T) {
	if got := ProjectKey("/home/user/projects/game"); got != "-home-user-projects-game" {
		t.Errorf("ProjectKey = %q", got)
	}
}

// TestSettingsLabIn: a project's lab_in becomes the sandbox's one write outside the project; a lab_in
// that is not a clean /mnt/... folder refuses the config.
func TestSettingsLabIn(t *testing.T) {
	dir := t.TempDir()
	write := func(labIn string) string {
		path := filepath.Join(dir, "projects.json")
		text, _ := json.Marshal(map[string]any{"projects": []any{map[string]any{"name": "game", "root": "/home/u/projects/game", "protected": []string{"Makefile"}, "lab_in": labIn}}})
		os.WriteFile(path, text, 0o644)
		return path
	}
	config, err := Load(write("/mnt/c/Users/u/agent-lab/game/lab-in"))
	if err != nil {
		t.Fatal(err)
	}
	filesystem := Settings(config)["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if got := filesystem["allowWrite"].([]string); len(got) != 1 || got[0] != "/mnt/c/Users/u/agent-lab/game/lab-in" {
		t.Errorf("allowWrite = %v", got)
	}
	if got := filesystem["denyWrite"].([]string); len(got) != 1 || got[0] != "/home/u/projects/game/Makefile" {
		t.Errorf("denyWrite = %v", got)
	}
	// Reads: no Windows drive but the lab exchange, no credential in the home, and only managed
	// settings may re-open a read.
	denyRead := filesystem["denyRead"].([]string)
	for _, want := range []string{"/mnt", "/home/u/.ssh", "/home/u/.claude/.credentials.json", "/home/u/.config/gh"} {
		if !slices.Contains(denyRead, want) {
			t.Errorf("denyRead lacks %s: %v", want, denyRead)
		}
	}
	if got := filesystem["allowRead"].([]string); len(got) != 1 || got[0] != "/mnt/c/Users/u/agent-lab/game" {
		t.Errorf("allowRead = %v", got)
	}
	if filesystem["allowManagedReadPathsOnly"] != true {
		t.Error("allowManagedReadPathsOnly is not on")
	}
	for _, bad := range []string{"/home/u/lab-in", "/mnt/c", "/mnt/c/x/../../etc", "relative/lab-in", "/mnt/c/Users/"} {
		if _, err := Load(write(bad)); err == nil {
			t.Errorf("lab_in %q accepted", bad)
		}
	}
	shipped, err := Load("../projects.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range shipped.Projects {
		if project.LabIn == "" {
			t.Errorf("shipped project %s has no lab_in", project.Name)
		}
	}
}
