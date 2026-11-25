// Command lab-run is /usr/local/bin/lab-run (install.sh builds it static and installs it root-owned):
// the way from WSL into the lab VM. It runs the Windows lab-run.exe, a client of the SYSTEM lab-broker,
// which proves the VM has no network before every call. Claude Code runs this one command outside the
// sandbox, so it does nothing but hand its arguments over:
//   - every argument base64-encoded (b64:...), so no quote, space or backslash is re-read on the way to
//     Windows, where a command line is one string the program splits again;
//   - the Linux working directory, so lab-run.exe can tell the project (wsl_root in its registry);
//   - a fixed environment: PATH, and the two variables WSL's interop needs, nothing of the caller's
//     (no WSLENV, so nothing reaches lab-run.exe either).
//
// It is a static Go binary, not a shell script: with no dynamic loader, LD_PRELOAD and its kind
// cannot put the caller's code into a program running outside the sandbox. Its exit code is
// lab-run.exe's (the process is replaced): the guest program's, 2 for a refusal, 64 for a usage error.
package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// program is the Windows lab-run (install\install.ps1). Only the tests build with another.
var program = "/mnt/c/Program Files/agent-containment/lab-run.exe"

// keep are the variables passed on, each only in the form WSL itself sets: interop finds its socket
// through WSL_INTEROP, and /init runs outside the sandbox, so a caller's socket is not passed.
var keep = map[string]*regexp.Regexp{
	"WSL_INTEROP":     regexp.MustCompile(`^/run/WSL/[0-9]+_interop$`),
	"WSL_DISTRO_NAME": regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`),
}

func main() {
	if sandboxed() {
		fmt.Fprintln(os.Stderr, "lab-run: this call ran inside the sandbox, which cannot reach Windows. Run lab-run as a command of its own, by its full path, with nothing else on the line: /usr/local/bin/lab-run ARGS (Windows paths in single quotes, so the shell keeps their backslashes).")
		os.Exit(2)
	}
	err := run(os.Args[1:])
	fmt.Fprintln(os.Stderr, "lab-run: "+err.Error())
	os.Exit(2)
}

// run replaces this process with lab-run.exe; it returns only on failure.
func run(arguments []string) error {
	if info, err := os.Stat(program); err != nil || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not installed (the Windows side: install\\install.ps1)", program)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	argv := []string{program, "--wsl", encode(cwd)}
	for _, argument := range arguments {
		argv = append(argv, encode(argument))
	}
	env := []string{"PATH=/usr/bin:/bin"}
	for _, name := range []string{"WSL_INTEROP", "WSL_DISTRO_NAME"} {
		if value, ok := os.LookupEnv(name); ok && keep[name].MatchString(value) {
			env = append(env, name+"="+value)
		}
	}
	// A Windows working directory, not \\wsl.localhost\...: lab-run.exe reads the Linux one above.
	if err := os.Chdir(filepath.Dir(program)); err != nil {
		return err
	}
	return syscall.Exec(program, argv, env)
}

// sandboxed reports a call Claude Code ran inside its sandbox (a compound line, a prefix): there WSL's
// interop is blocked and lab-run.exe would fail with only "UtilBindVsockAnyPort ... socket failed".
// SANDBOX_RUNTIME alone is not taken on its word: interop's socket must also refuse a connection.
func sandboxed() bool {
	if os.Getenv("SANDBOX_RUNTIME") != "1" {
		return false
	}
	socket := os.Getenv("WSL_INTEROP")
	if socket == "" {
		return true
	}
	connection, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return true
	}
	connection.Close()
	return false
}

func encode(text string) string { return "b64:" + base64.StdEncoding.EncodeToString([]byte(text)) }
