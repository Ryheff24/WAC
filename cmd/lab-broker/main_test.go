package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ryheff24/WAC/lab"
)

// The suite builds lab-broker (with a fake VM) and lab-run as the agent would use them, starts the
// broker on a private pipe, and drives lab-run against it. With WAC_COVERDIR set, both are built with
// -cover and write what they ran there.

type world struct {
	dir, root, stateDir, registryDir, pipe string
	broker, labRun                         string
}

func build(t *testing.T, pkg, output string, ldflags map[string]string) {
	t.Helper()
	arguments := []string{"build", "-o", output}
	if os.Getenv("WAC_COVERDIR") != "" {
		arguments = append(arguments, "-cover", "-coverpkg=github.com/Ryheff24/WAC/...")
	}
	var flags []string
	for key, value := range ldflags {
		flags = append(flags, "-X", "main."+key+"="+value)
	}
	arguments = append(arguments, "-ldflags", strings.Join(flags, " "), pkg)
	if out, err := exec.Command("go", arguments...).CombinedOutput(); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(arguments, " "), err, out)
	}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{dir: dir, root: filepath.Join(dir, "proj"), stateDir: filepath.Join(dir, "state"), registryDir: filepath.Join(dir, "registry"),
		pipe: fmt.Sprintf(`\\.\pipe\agent-containment-e2e-%d`, time.Now().UnixNano())}
	for _, d := range []string{filepath.Join(w.root, "prior-art"), filepath.Join(w.stateDir, "labs"), w.registryDir} {
		os.MkdirAll(d, 0o755)
	}
	entry, _ := json.Marshal(map[string]any{"name": "proj", "root": w.root, "wsl_root": "/home/u/proj", "lab_vms": []string{"proj-lab"}})
	os.WriteFile(filepath.Join(w.registryDir, "proj.json"), entry, 0o644)
	account, _ := user.Current()
	state, _ := json.Marshal(lab.State{VM: "proj-lab", Project: "proj", Hostname: "PROJ-LAB", ClientSID: account.Uid,
		Disks: []string{`C:\vm\os-diff.vhdx`}, OSBase: `C:\vm\os-base.vhdx`, OSDiff: `C:\vm\os-diff.vhdx`, Credential: `C:\vm\c`, GuestUser: "lab"})
	os.WriteFile(filepath.Join(w.stateDir, "labs", "proj-lab.json"), state, 0o644)
	w.broker = filepath.Join(dir, "bin", "lab-broker.exe")
	w.labRun = filepath.Join(dir, "bin", "lab-run.exe")
	build(t, ".", w.broker, map[string]string{"registryDirectory": w.registryDir, "stateDirectory": w.stateDir, "pipeName": w.pipe, "hypervisorKind": "fake"})
	build(t, "../lab-run", w.labRun, map[string]string{"registryDirectory": w.registryDir, "pipeName": w.pipe, "brokerImage": w.broker})
	return w
}

func (w *world) command(t *testing.T, program string, arguments ...string) *exec.Cmd {
	command := exec.Command(program, arguments...)
	command.Dir = w.root
	command.Env = os.Environ()
	if coverage := os.Getenv("WAC_COVERDIR"); coverage != "" {
		command.Env = append(command.Env, "GOCOVERDIR="+coverage)
	}
	return command
}

func (w *world) run(t *testing.T, program string, arguments ...string) (string, string, int) {
	t.Helper()
	command := w.command(t, program, arguments...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

func TestEndToEnd(t *testing.T) {
	w := newWorld(t)
	serve := w.command(t, w.broker, "serve")
	var serveOut bytes.Buffer
	serve.Stdout, serve.Stderr = &serveOut, &serveOut
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			serve.Process.Kill()
		}
	}()
	for i := 0; i < 50; i++ {
		if _, _, code := w.run(t, w.labRun, "status"); code == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	check := func(name string, wantCode int, wantOut string, arguments ...string) {
		t.Helper()
		stdout, stderr, code := w.run(t, w.labRun, arguments...)
		if code != wantCode || !strings.Contains(stdout+stderr, wantOut) {
			t.Errorf("%s: lab-run %q = exit %d, stdout %q, stderr %q; want exit %d and %q", name, arguments, code, stdout, stderr, wantCode, wantOut)
		}
	}
	check("status", 0, "contained", "status")
	check("run", 0, "ran hostname", "--", "hostname")
	check("run with options", 0, `in "C:\\work"`, "--project", "proj", "--vm", "proj-lab", "--timeout", "60", "--cwd", `C:\work`, "--", "tool", "-x")
	check("exit code", 5, "", "--", "exit", "5")
	check("stderr", 0, "warning", "--", "stderr", "warning")
	os.WriteFile(filepath.Join(w.root, "prior-art", "data.bin"), []byte("d"), 0o644)
	check("push", 0, "pushed", "push", `prior-art\data.bin`)
	check("push from outside the project", 2, "is not inside", "push", filepath.Join(w.dir, "state", "labs", "proj-lab.json"))
	guestOut := filepath.Join(w.stateDir, "fake-guest", "proj-lab", "C", "out")
	os.MkdirAll(guestOut, 0o755)
	os.WriteFile(filepath.Join(guestOut, "song.ogg"), []byte("ogg"), 0o644)
	check("pull", 0, "marked as from the internet", "pull", `C:\out`)
	check("reset", 0, "contained", "reset")
	check("unknown project", 2, "not registered", "--project", "nope", "status")

	os.MkdirAll(filepath.Join(w.root, "lab-in"), 0o755)
	os.WriteFile(filepath.Join(w.root, "lab-in", "Upstream.bundle"), []byte("bundle"), 0o644)
	check("sync", 0, "ran powershell", "sync", "Upstream")
	if _, err := os.Stat(filepath.Join(w.stateDir, "fake-guest", "proj-lab", "C", "inbox", "Upstream.bundle")); err != nil {
		t.Errorf("sync did not push the bundle: %v", err)
	}
	check("sync of nothing", 2, "no bundle at", "sync", "Missing")

	// From WSL (wsl/labrun): the project from wsl_root, arguments exactly as given, exit codes,
	// and a push path in the Linux file system refused with the way through lab-in.
	wsl := func(cwd string, arguments ...string) []string {
		out := []string{"--wsl"}
		for _, word := range append([]string{cwd}, arguments...) {
			out = append(out, "b64:"+base64.StdEncoding.EncodeToString([]byte(word)))
		}
		return out
	}
	check("WSL status", 0, "contained", wsl("/home/u/proj/sub", "status")...)
	check("WSL run", 0, `ran tool a b "q"`, wsl("/home/u/proj", "--", "tool", "a b", `"q"`)...)
	check("WSL exit code", 3, "", wsl("/home/u/proj", "--", "exit", "3")...)
	check("WSL outside every project", 2, "in no registered project", wsl("/home/u", "status")...)
	check("WSL push from Linux", 2, "copy it into", wsl("/home/u/proj", "push", "prior-art/x")...)
	check("WSL pull", 0, "/lab-out/", wsl("/home/u/proj", "pull", `C:\out`)...)

	// The negative control: give the VM an adapter, and every call is refused.
	os.WriteFile(filepath.Join(w.stateDir, "fake-adapter"), nil, 0o644)
	check("an adapter", 2, "REFUSED: VM proj-lab has 1 network adapters", "--", "hostname")
	check("sync refused", 2, "REFUSED", "sync", "Upstream")
	os.Remove(filepath.Join(w.stateDir, "fake-adapter"))
	check("adapter removed", 0, "contained", "status")

	logs, _ := filepath.Glob(filepath.Join(w.stateDir, "log", "broker-*.log"))
	if len(logs) != 1 {
		t.Errorf("want one broker log, got %v", logs)
	} else if text, _ := os.ReadFile(logs[0]); !strings.Contains(string(text), `"argv":["hostname"]`) || !strings.Contains(string(text), `"argv":["tool","a b","\"q\""]`) {
		t.Errorf("the log lacks the run: %s", text)
	}

	os.WriteFile(filepath.Join(w.stateDir, "fake-stop"), nil, 0o644)
	time.Sleep(300 * time.Millisecond)
	w.run(t, w.labRun, "status") // wakes the pipe so serve sees the stop
	done := make(chan error, 1)
	go func() { done <- serve.Wait() }()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Errorf("serve exited with %v: %s", err, serveOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Errorf("serve did not stop: %s", serveOut.String())
	}
}

func TestBrokerCommands(t *testing.T) {
	w := newWorld(t)
	stdout, _, code := w.run(t, w.broker, "check")
	if code != 2 || !strings.Contains(stdout, "FAIL the hypervisor is Hyper-V (this build: fake)") || !strings.Contains(stdout, "OK   lab state proj-lab") {
		t.Errorf("check: exit %d %q", code, stdout)
	}
	for _, args := range [][]string{{}, {"frob"}, {"set-owner"}, {"serve", "x"}} {
		if _, _, code := w.run(t, w.broker, args...); code != 64 {
			t.Errorf("lab-broker %v: exit %d, want 64", args, code)
		}
	}
	os.Remove(filepath.Join(w.stateDir, "labs", "proj-lab.json"))
	if _, stderr, code := w.run(t, w.broker, "serve"); code != 1 || !strings.Contains(stderr, "no lab states") {
		t.Errorf("serve without states: exit %d %q", code, stderr)
	}
	if stdout, _, code := w.run(t, w.broker, "check"); code != 2 || !strings.Contains(stdout, "FAIL lab states") {
		t.Errorf("check without states: exit %d %q", code, stdout)
	}
	os.RemoveAll(w.registryDir)
	if _, stderr, code := w.run(t, w.broker, "serve"); code != 1 || !strings.Contains(stderr, "registry") {
		t.Errorf("serve without a registry: exit %d %q", code, stderr)
	}
}

func TestOwnerSID(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "labs"), 0o755)
	write := func(vm, sid string) {
		text, _ := json.Marshal(lab.State{VM: vm, Project: "p", Hostname: "H", ClientSID: sid, Disks: []string{"d"}, OSBase: "b", OSDiff: "d", Credential: "c", GuestUser: "u"})
		os.WriteFile(filepath.Join(dir, "labs", vm+".json"), text, 0o644)
	}
	write("a", "S-1-5-21-1-2-3-1001")
	if sid, err := ownerSID(dir); err != nil || sid != "S-1-5-21-1-2-3-1001" {
		t.Errorf("ownerSID = %q, %v", sid, err)
	}
	write("b", "S-1-5-21-1-2-3-1002")
	if _, err := ownerSID(dir); err == nil {
		t.Error("two owners accepted")
	}
	os.WriteFile(filepath.Join(dir, "labs", "c.json"), []byte("{"), 0o644)
	if _, err := allStates(dir); err == nil {
		t.Error("an unreadable state accepted")
	}
	empty := t.TempDir()
	os.MkdirAll(filepath.Join(empty, "labs"), 0o755)
	if _, err := allStates(empty); err == nil {
		t.Error("no states accepted")
	}
	if _, ok := hypervisor().(lab.HyperV); !ok {
		t.Error("the default build's hypervisor is not Hyper-V")
	}
}

// TestSetOwner: every state gets the owner's SID, nothing else in it changes (the game disk's recorded
// write time to the 100 ns), a second run changes nothing, and a bad SID changes no file.
func TestSetOwner(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "labs"), 0o755)
	modified, _ := time.Parse(time.RFC3339Nano, "2026-11-21T22:14:06.5046447Z")
	state := lab.State{VM: "game-lab", Project: "game", Hostname: "GAME-LAB", ClientSID: "S-1-5-21-1-2-3-1011",
		Disks: []string{`C:\vm\os-diff.vhdx`, `C:\vm\game-diff.vhdx`}, OSBase: `C:\vm\os-base.vhdx`, OSDiff: `C:\vm\os-diff.vhdx`,
		Credential: `C:\vm\c`, GuestUser: "lab", GameParent: &lab.FileFact{Path: `D:\game.vhdx`, Size: 100000000000, Modified: modified},
		GameDiff: `C:\vm\game-diff.vhdx`}
	text, _ := json.MarshalIndent(state, "", "  ")
	path := filepath.Join(dir, "labs", "game-lab.json")
	os.WriteFile(path, text, 0o644)

	var out bytes.Buffer
	if err := setOwner(dir, "bad-sid", &out); err == nil {
		t.Error("a bad SID accepted")
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, text) {
		t.Error("a refused set-owner changed the state")
	}
	if err := setOwner(dir, "S-1-5-21-1-2-3-1001", &out); err != nil || !strings.Contains(out.String(), "set to S-1-5-21-1-2-3-1001") {
		t.Fatalf("set-owner: %v %q", err, out.String())
	}
	got, err := lab.LoadState(dir, "game-lab")
	if err != nil {
		t.Fatal(err)
	}
	want := state
	want.ClientSID = "S-1-5-21-1-2-3-1001"
	// %+v of a State would print GameParent's address: compare the pointed-to facts separately.
	flat := func(s lab.State) string {
		parent := *s.GameParent
		s.GameParent = nil
		return fmt.Sprintf("%+v %+v", s, parent)
	}
	if a, b := flat(got), flat(want); a != b || !got.GameParent.Modified.Equal(modified) {
		t.Errorf("after set-owner:\n%s\nwant\n%s", a, b)
	}
	if sid, err := ownerSID(dir); err != nil || sid != "S-1-5-21-1-2-3-1001" {
		t.Errorf("ownerSID after set-owner = %q %v", sid, err)
	}
	out.Reset()
	if err := setOwner(dir, "S-1-5-21-1-2-3-1001", &out); err != nil || !strings.Contains(out.String(), "already") {
		t.Errorf("a second set-owner: %v %q", err, out.String())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "labs", "*.new")); len(leftovers) != 0 {
		t.Errorf("left behind: %v", leftovers)
	}
	if err := setOwner(t.TempDir(), "S-1-5-21-1-2-3-1001", &out); err == nil {
		t.Error("set-owner with no states succeeded")
	}
}
