package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ryheff24/WAC/lab"
	"github.com/Ryheff24/WAC/registry"
)

func TestParse(t *testing.T) {
	good := map[string]lab.Request{
		"-- a b": {Verb: lab.VerbRun, Argv: []string{"a", "b"}},
		"--project p --vm v --timeout 9 --cwd C:\\w -- x": {Verb: lab.VerbRun, Project: "p", VM: "v", Timeout: 9, Cwd: `C:\w`, Argv: []string{"x"}},
		"status":       {Verb: lab.VerbStatus},
		"reset":        {Verb: lab.VerbReset},
		"push f":       {Verb: lab.VerbPush, HostPath: "f"},
		"pull C:\\out": {Verb: lab.VerbPull, Guest: `C:\out`},
	}
	for line, want := range good {
		got, repo, why := parse(strings.Fields(line))
		if why != "" || repo != "" || got.Verb != want.Verb || got.Project != want.Project || got.VM != want.VM ||
			got.Timeout != want.Timeout || got.Cwd != want.Cwd || got.HostPath != want.HostPath || got.Guest != want.Guest ||
			strings.Join(got.Argv, " ") != strings.Join(want.Argv, " ") {
			t.Errorf("parse(%q) = %+v %q %q, want %+v", line, got, repo, why, want)
		}
	}
	if _, repo, why := parse([]string{"sync", "Upstream"}); repo != "Upstream" || why != "" {
		t.Errorf("sync: %q %q", repo, why)
	}
	for _, line := range []string{"", "--", "--project", "--timeout x status", "--timeout -1 status", "--frob status", "status extra",
		"push", "sync ../evil", "sync Upstream.bundle", "frob", "--vm"} {
		if _, _, why := parse(strings.Fields(line)); why == "" {
			t.Errorf("parse(%q) accepted", line)
		}
	}
}

func TestRunRefusals(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"frob"}, &out, &errs); code != 64 || !strings.Contains(errs.String(), "usage") {
		t.Errorf("a bad verb: %d %q", code, errs.String())
	}
	registryDirectory = filepath.Join(t.TempDir(), "none")
	errs.Reset()
	if code := run([]string{"status"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "registry") {
		t.Errorf("no registry: %d %q", code, errs.String())
	}
	projects := []registry.Project{{Name: "p", Root: `C:\nowhere-at-all`, LabVMs: []string{"v"}}}
	if _, why := projectFor(projects, "q", ""); why == "" {
		t.Error("an unregistered project accepted")
	}
	if p, why := projectFor(projects, "p", ""); why != "" || p.Name != "p" {
		t.Errorf("projectFor(p) = %v %q", p, why)
	}
	if _, why := projectFor(projects, "", ""); !strings.Contains(why, "no registered project") {
		t.Errorf("the current directory is in no project: %q", why)
	}
	cwd, _ := os.Getwd()
	here := []registry.Project{{Name: "here", Root: cwd, LabVMs: []string{"v"}}}
	if p, why := projectFor(here, "", ""); why != "" || p.Name != "here" {
		t.Errorf("projectFor(cwd) = %v %q", p, why)
	}
}

func b64(words ...string) []string {
	out := []string{}
	for _, word := range words {
		out = append(out, "b64:"+base64.StdEncoding.EncodeToString([]byte(word)))
	}
	return out
}

// TestDecodeWSL: what wsl/labrun passes (its own tests cover the encoding side) decodes exactly.
func TestDecodeWSL(t *testing.T) {
	arguments := []string{"--", "powershell", "-Command", `Write-Host "a b" 'c' \\x\`, "", "two\nlines"}
	cwd, got, err := decodeWSL(b64(append([]string{"/home/u/projects/game"}, arguments...)...))
	if err != nil || cwd != "/home/u/projects/game" || strings.Join(got, "|") != strings.Join(arguments, "|") || len(got) != len(arguments) {
		t.Errorf("decodeWSL = %q %q %v", cwd, got, err)
	}
	for _, bad := range [][]string{{}, {"/home/u"}, append(b64("/home/u"), "plain"), append(b64("/home/u"), "b64:!!"), b64("relative")} {
		if _, _, err := decodeWSL(bad); err == nil {
			t.Errorf("decodeWSL(%q) accepted", bad)
		}
	}
	var out, errs bytes.Buffer
	if code := run(append([]string{wslFlag}, "plain"), &out, &errs); code != 64 {
		t.Errorf("a bad --wsl call: exit %d %q", code, errs.String())
	}
}

// TestWSLPaths: from WSL, the project comes from wsl_root (or the lab exchange's /mnt/c spelling), and
// a push path must be on a Windows drive.
func TestWSLPaths(t *testing.T) {
	game := registry.Project{Name: "game", Root: `C:\Users\u\agent-lab\game`, WSLRoot: "/home/u/projects/game", LabVMs: []string{"game-lab"}}
	projects := []registry.Project{game}
	for _, cwd := range []string{"/home/u/projects/game", "/home/u/projects/game/prior-art/x", "/mnt/c/Users/u/agent-lab/game/lab-in"} {
		if p, why := projectFor(projects, "", cwd); why != "" || p.Name != "game" {
			t.Errorf("projectFor(%s) = %v %q", cwd, p.Name, why)
		}
	}
	for _, cwd := range []string{"/home/u/projects/gamex", "/home/u", "/mnt/c/Users/u"} {
		if _, why := projectFor(projects, "", cwd); why == "" {
			t.Errorf("projectFor(%s) found a project", cwd)
		}
	}
	paths := map[[2]string]string{
		{"/mnt/c/Users/u/agent-lab/game/lab-in/x.zip", "/home/u/projects/game"}: `C:\Users\u\agent-lab\game\lab-in\x.zip`,
		{"x.zip", "/mnt/c/Users/u/agent-lab/game/lab-in"}:                       `C:\Users\u\agent-lab\game\lab-in\x.zip`,
		{`C:\Users\u\agent-lab\game\lab-in\y`, "/home/u/projects/game"}:         `C:\Users\u\agent-lab\game\lab-in\y`,
	}
	for in, want := range paths {
		if got, why := hostPath(game, in[0], in[1]); why != "" || !strings.EqualFold(got, want) {
			t.Errorf("hostPath(%q from %s) = %q %q, want %q", in[0], in[1], got, why, want)
		}
	}
	for _, in := range [][2]string{{"prior-art/x", "/home/u/projects/game"}, {"/home/u/.ssh/id_ed25519", "/home/u/projects/game"}} {
		if got, why := hostPath(game, in[0], in[1]); !strings.Contains(why, "/mnt/c/Users/u/agent-lab/game/lab-in") {
			t.Errorf("hostPath(%q) = %q %q, want the way through lab-in", in[0], got, why)
		}
	}
}

func TestSyncScript(t *testing.T) {
	script := syncScript("Upstream", "0a1b2c3d")
	for _, want := range []string{`git clone --quiet 'C:\inbox\Upstream.bundle' 'C:\work\.sync-0a1b2c3d\Upstream'`, `Move-Item 'C:\work\.sync-0a1b2c3d\Upstream' 'C:\work\Upstream'`} {
		if !strings.Contains(script, want) {
			t.Errorf("the sync script lacks %s:\n%s", want, script)
		}
	}
}

// TestSpellingWriter: from WSL, the exchange's Windows paths come out in WSL's spelling, new folder
// included; other text is untouched.
func TestSpellingWriter(t *testing.T) {
	var out bytes.Buffer
	w := newSpellingWriter(&out, `C:\Users\u\agent-lab\game`, "/mnt/c/Users/u/agent-lab/game")
	for in, want := range map[string]string{
		"pulled C:\\out to C:\\Users\\u\\agent-lab\\game\\lab-out\\20261122-0136-ab: 2 files\n": "pulled C:\\out to /mnt/c/Users/u/agent-lab/game/lab-out/20261122-0136-ab: 2 files\n",
		"c:\\users\\U\\AGENT-LAB\\game\\x and C:\\Users\\u\\agent-lab\\game":                    "/mnt/c/Users/u/agent-lab/game/x and /mnt/c/Users/u/agent-lab/game",
		// Letters whose lower case has another length, before the path: the text stays whole.
		"İİİ Ⱥ C:\\Users\\u\\agent-lab\\game\\lab-out\\y ok": "İİİ Ⱥ /mnt/c/Users/u/agent-lab/game/lab-out/y ok",
		"no path here ⱥ": "no path here ⱥ",
	} {
		out.Reset()
		w.Write([]byte(in))
		if out.String() != want {
			t.Errorf("got  %q\nwant %q", out.String(), want)
		}
	}
}

// TestWSLExit: from WSL, a code that does not fit in a Linux exit status is printed whole and never
// becomes 0 (success); codes that fit pass unchanged.
func TestWSLExit(t *testing.T) {
	for code, want := range map[int]int{0: 0, 1: 1, 2: 2, 64: 64, 255: 255, -196608: 1, 256: 1, 0x10005: 5, -1073741819: 5, -1: 255} {
		var errs bytes.Buffer
		if got := wslExit(code, &errs); got != want {
			t.Errorf("wslExit(%d) = %d, want %d", code, got, want)
		}
		if fits := code >= 0 && code <= 255; fits == strings.Contains(errs.String(), "exited with") {
			t.Errorf("wslExit(%d) printed %q", code, errs.String())
		}
	}
}
