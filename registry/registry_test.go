package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// windowsOnly skips a test of Windows path semantics (filepath on Windows) elsewhere; CI and the
// install run the suite on Windows.
func windowsOnly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path semantics")
	}
}

func TestNormalize(t *testing.T) {
	windowsOnly(t)
	cases := map[string]string{
		`/c/Users/x/game`:      `C:\Users\x\game`,
		`C:/Users/x/game/`:     `C:\Users\x\game`,
		`c:\Users\x\..\y`:      `C:\Users\y`,
		`\\?\C:\Program Files`: `C:\Program Files`,
		`/d`:                   `D:\`,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithin(t *testing.T) {
	windowsOnly(t)
	yes := [][2]string{{`C:\a\b`, `c:\A\B\c`}, {`C:\a\b`, `/c/a/b/c`}, {`C:\a\b`, `C:\a\b`}}
	no := [][2]string{{`C:\a\b`, `C:\a\bc`}, {`C:\a\b`, `C:\a`}, {`C:\a\b`, `D:\a\b`}, {`C:\a`, ``}}
	for _, c := range yes {
		if !Within(c[0], c[1]) {
			t.Errorf("Within(%q, %q) = false", c[0], c[1])
		}
	}
	for _, c := range no {
		if Within(c[0], c[1]) {
			t.Errorf("Within(%q, %q) = true", c[0], c[1])
		}
	}
}

func TestWithinLinux(t *testing.T) {
	yes := [][2]string{{"/home/u/p", "/home/u/p"}, {"/home/u/p", "/home/u/p/a/b"}, {"/home/u/p/", "/home/u/p/a"}, {"/home/u/p", "/home/u/p/a/../b"}}
	no := [][2]string{{"/home/u/p", "/home/u/pq"}, {"/home/u/p", "/home/u"}, {"/home/u/p", "/home/u/p/../q"}, {"/home/u/p", "relative"},
		{"", "/x"}, {"/home/u/p", "/HOME/u/p"}}
	for _, c := range yes {
		if !WithinLinux(c[0], c[1]) {
			t.Errorf("WithinLinux(%q, %q) = false", c[0], c[1])
		}
	}
	for _, c := range no {
		if WithinLinux(c[0], c[1]) {
			t.Errorf("WithinLinux(%q, %q) = true", c[0], c[1])
		}
	}
}

func TestFromAndToWSL(t *testing.T) {
	from := map[string]string{
		"/mnt/c/Users/r/agent-lab/game/lab-in/x.zip": `C:\Users\r\agent-lab\game\lab-in\x.zip`,
		"/mnt/d":           `D:\`,
		"/mnt/c/a/../b":    `C:\b`,
		"/mnt/c/a b/c.txt": `C:\a b\c.txt`,
	}
	for in, want := range from {
		if got, ok := FromWSL(in); !ok || got != want {
			t.Errorf("FromWSL(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"/home/r/projects/game/x", "/mnt/wsl/x", "/mnt/C/x", "/mnt", "relative/x", "/mnt/c/../../etc/passwd", "", `/mnt/c/Users/r/lab-in/..\..\x`, "/mnt/c/a:stream"} {
		if got, ok := FromWSL(in); ok {
			t.Errorf("FromWSL(%q) = %q, want refused", in, got)
		}
	}
	to := map[string]string{`C:\Users\r\agent-lab\game\lab-out`: "/mnt/c/Users/r/agent-lab/game/lab-out", `D:\`: "/mnt/d", "C:/a/b": "/mnt/c/a/b"}
	for in, want := range to {
		if got := ToWSL(in); got != want {
			t.Errorf("ToWSL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	bad := []string{
		`{"name":"game","root":"C:\\x","lab_vms":["v"],"surprise":1}`,
		`{"name":"game","root":"relative","lab_vms":["v"]}`,
		`{"name":"game","root":"C:\\x\\..\\y","lab_vms":["v"]}`,
		`{"name":"Bad Name","root":"C:\\x","lab_vms":["v"]}`,
		`{"name":"game","root":"C:\\x","lab_vms":[]}`,
		`{"name":"game","root":"C:\\x","lab_vms":["bad name"]}`,
		`{"name":"game","root":"C:\\x","lab_vms":["v"],"wsl_root":"relative"}`,
		`{"name":"game","root":"C:\\x","lab_vms":["v"],"wsl_root":"/home/u/../p"}`,
		`{"name":"game","root":"C:\\x","lab_vms":["v"],"wsl_root":"/"}`,
		`{"name":"game","root":"C:\\x","lab_vms":["v"],"untrusted":["a"]}`,
		`{"name":"game","root":"C:\\x","lab_vms":["v"]} {}`,
	}
	for _, text := range bad {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("Parse(%s) accepted", text)
		}
	}
	good, err := Parse([]byte(`{"name":"game","root":"C:\\x","wsl_root":"/home/u/p","lab_vms":["game-lab"]}`))
	if err != nil || good.WSLRoot != "/home/u/p" {
		t.Errorf("a good project refused: %v", err)
	}
}

// TestShippedProjects: the entries install.ps1 copies to the registry directory parse and validate,
// and each has a WSL root.
func TestShippedProjects(t *testing.T) {
	projects, err := Load("projects")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		if p.WSLRoot == "" {
			t.Errorf("%s has no wsl_root", p.Name)
		}
	}
	if p, ok := ContainingWSL(projects, "/home/user/projects/game/prior-art"); !ok || p.Name != "game" {
		t.Errorf("ContainingWSL(game/prior-art) = %v %v", p.Name, ok)
	}
}

func TestLoad(t *testing.T) {
	directory := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("game.json", `{"name":"game","root":"C:\\p\\game","wsl_root":"/home/u/game","lab_vms":["game-lab"]}`)
	if projects, err := Load(directory); err != nil || len(projects) != 1 {
		t.Fatalf("Load = %v, %v", projects, err)
	}
	if runtime.GOOS == "windows" {
		write("nested.json", `{"name":"nested","root":"C:\\p\\game\\inner","lab_vms":["x"]}`)
		if _, err := Load(directory); err == nil {
			t.Error("nested roots accepted")
		}
		os.Remove(filepath.Join(directory, "nested.json"))
	}
	write("wslnest.json", `{"name":"wslnest","root":"C:\\q","wsl_root":"/home/u/game/sub","lab_vms":["x"]}`)
	if _, err := Load(directory); err == nil {
		t.Error("nested WSL roots accepted")
	}
	os.Remove(filepath.Join(directory, "wslnest.json"))
	write("other.json", `{"name":"mismatch","root":"C:\\q","lab_vms":["x"]}`)
	if _, err := Load(directory); err == nil {
		t.Error("a name that does not match its file accepted")
	}
	os.Remove(filepath.Join(directory, "other.json"))
	write("bad.json", `{`)
	if _, err := Load(directory); err == nil {
		t.Error("an unreadable file accepted")
	}
	if _, err := Load(filepath.Join(directory, "missing")); err == nil {
		t.Error("a missing registry accepted")
	}
	empty := t.TempDir()
	if _, err := Load(empty); err == nil {
		t.Error("an empty registry accepted")
	}
}
