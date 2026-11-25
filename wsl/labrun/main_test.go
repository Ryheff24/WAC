package main

import (
	"debug/elf"
	"encoding/base64"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// build builds lab-run as install.sh does (static), running fake (a shell script) instead of
// lab-run.exe; an empty fake is a missing lab-run.exe.
func build(t *testing.T, fake string) string {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "Program Files", "lab-run.exe")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	if fake != "" {
		os.WriteFile(exe, []byte("#!/bin/sh\n"+fake), 0o755)
	}
	binary := filepath.Join(dir, "lab-run")
	command := exec.Command("go", "build", "-ldflags", "-X 'main.program="+exe+"'", "-o", binary, ".")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binary
}

// TestArgumentsArriveExactly: what lab-run.exe receives decodes to exactly what the caller passed
// (lab-run.exe's decoding is tested on Windows, cmd/lab-run), its exit code comes back, and none of
// the caller's environment reaches it.
func TestArgumentsArriveExactly(t *testing.T) {
	out := filepath.Join(t.TempDir(), "argv")
	labRun := build(t, `pwd > "`+out+`"; for a; do printf '%s\n' "$a" >> "`+out+`"; done; env > "`+out+`.env"; exit 7`)
	arguments := []string{"--", "powershell", "-Command", `Write-Host "a b" 'c'; $x = "\\server\share\"`, "", "two\nlines", `C:\a b\`, "ünïcødé", "$(touch /tmp/x)", "`id`", "*", "-"}
	project := t.TempDir()
	command := exec.Command(labRun, arguments...)
	command.Dir = project
	command.Env = append(environ(), "WSLENV=PATH/l:EVIL", "PATH=/nowhere:"+os.Getenv("PATH"), "LD_PRELOAD=/nowhere/evil.so",
		"WSL_INTEROP=/run/WSL/1_interop", "SECRET=x")
	err := command.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Fatalf("exit: %v, want the fake's 7", err)
	}
	text, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) != 3+len(arguments) || lines[1] != "--wsl" {
		t.Fatalf("the fake got %q", lines)
	}
	if !strings.HasSuffix(lines[0], "Program Files") {
		t.Errorf("lab-run.exe's working directory is %s, want its own directory", lines[0])
	}
	decode := func(word string) string {
		text, ok := strings.CutPrefix(word, "b64:")
		raw, err := base64.StdEncoding.DecodeString(text)
		if !ok || err != nil {
			t.Fatalf("%q is not b64: (%v)", word, err)
		}
		return string(raw)
	}
	if got := decode(lines[2]); got != project {
		t.Errorf("working directory %q, want %q", got, project)
	}
	for i, want := range arguments {
		if got := decode(lines[3+i]); got != want {
			t.Errorf("argument %d: %q, want %q", i, got, want)
		}
	}
	env, _ := os.ReadFile(out + ".env")
	for _, leaked := range []string{"WSLENV", "/nowhere", "LD_PRELOAD", "SECRET"} {
		if strings.Contains(string(env), leaked) {
			t.Errorf("%s reached lab-run.exe:\n%s", leaked, env)
		}
	}
	if !strings.Contains(string(env), "WSL_INTEROP=/run/WSL/1_interop") {
		t.Errorf("WSL_INTEROP, which interop needs, did not reach lab-run.exe:\n%s", env)
	}

	// A WSL_INTEROP not in WSL's own form (a socket the caller made) is dropped.
	command = exec.Command(labRun, "status")
	command.Env = append(environ(), "WSL_INTEROP=/tmp/mine.sock", "WSL_DISTRO_NAME=Ubuntu")
	command.Run()
	env, _ = os.ReadFile(out + ".env")
	if strings.Contains(string(env), "WSL_INTEROP") || !strings.Contains(string(env), "WSL_DISTRO_NAME=Ubuntu") {
		t.Errorf("a caller's WSL_INTEROP passed, or WSL_DISTRO_NAME did not:\n%s", env)
	}
}

func TestNoArgumentsAndMissingProgram(t *testing.T) {
	out := filepath.Join(t.TempDir(), "count")
	count := exec.Command(build(t, `printf '%s\n' "$#" > "`+out+`"`))
	count.Env = environ()
	if err := count.Run(); err != nil {
		t.Fatal(err)
	}
	if text, _ := os.ReadFile(out); string(text) != "2\n" {
		t.Errorf("with no arguments the fake got %q arguments, want 2 (--wsl and the directory)", text)
	}
	missing := exec.Command(build(t, ""))
	missing.Env = environ()
	output, err := missing.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "not installed") {
		t.Errorf("without lab-run.exe: %v %q, want exit 2 and 'not installed'", err, output)
	}
}

// TestStatic: built as install.sh builds it, lab-run has no program interpreter (no dynamic loader),
// so LD_PRELOAD cannot reach it.
func TestStatic(t *testing.T) {
	file, err := elf.Open(build(t, "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			t.Error("lab-run has a program interpreter: it is not static")
		}
	}
}

// environ is the test's environment without SANDBOX_RUNTIME (the tests may themselves run sandboxed).
func environ() []string {
	var out []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SANDBOX_RUNTIME=") {
			out = append(out, entry)
		}
	}
	return out
}

// TestSandboxedHint: run inside the sandbox (SANDBOX_RUNTIME=1 and interop's socket refusing), lab-run
// says how to call it instead of failing on the socket, and does not run lab-run.exe; with the socket
// answering it runs (the control, where this test may listen on a socket).
func TestSandboxedHint(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	labRun := build(t, `touch "`+ran+`"`)
	command := exec.Command(labRun, "status")
	command.Env = append(environ(), "SANDBOX_RUNTIME=1", "WSL_INTEROP="+filepath.Join(t.TempDir(), "no-socket"))
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "full path") {
		t.Errorf("sandboxed: %v %q, want exit 2 and the hint", err, output)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("lab-run.exe ran from inside the sandbox")
	}
	socket := filepath.Join(t.TempDir(), "interop")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("cannot listen on a unix socket here (%v): the control runs in CI", err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()
	command = exec.Command(labRun, "status")
	command.Env = append(environ(), "SANDBOX_RUNTIME=1", "WSL_INTEROP="+socket)
	if output, err := command.CombinedOutput(); err != nil {
		t.Errorf("with interop answering: %v %q", err, output)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Error("with interop answering, lab-run.exe did not run")
	}
}
