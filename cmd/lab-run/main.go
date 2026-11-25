// Command lab-run is the way into a lab VM: a client of lab-broker's pipe. It decides nothing; the
// broker proves the VM's containment and checks every path. lab-run refuses to talk to any server but
// the installed lab-broker.exe (a process squatting on the pipe's name is not it).
//
//	lab-run [--project P] [--vm V] [--timeout SECONDS] [--cwd GUESTDIR] -- PROGRAM ARGS...
//	lab-run [--project P] [--vm V] status | reset
//	lab-run [--project P] [--vm V] push HOSTPATH   copy from the project's lab exchange into the guest's C:\inbox
//	lab-run [--project P] [--vm V] pull GUESTPATH  copy into the lab exchange's lab-out\<time>
//	lab-run [--project P] [--vm V] sync REPO       clone lab-in\REPO.bundle into the guest's C:\work\REPO
//
// The project is the registered one holding the current directory, unless --project names one. The
// exit code is the guest program's, or 2 for a refusal, or 64 for a usage error.
//
// From WSL, /usr/local/bin/lab-run (wsl/labrun) runs this program as
//
//	lab-run.exe --wsl b64:CWD b64:ARG...
//
// with the Linux working directory and every argument base64-encoded, so nothing is re-quoted on the
// way to Windows. Then the project is the one whose wsl_root holds CWD, and a push path may be a
// /mnt/c/... path (or relative to CWD and on a Windows drive): the broker reads only Windows paths
// inside the project's lab exchange.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Ryheff24/WAC/lab"
	"github.com/Ryheff24/WAC/registry"
)

var (
	registryDirectory = registry.Directory
	pipeName          = lab.PipeName
	brokerImage       = `C:\Program Files\agent-containment\lab-broker.exe`
)

const usage = `usage: lab-run [--project P] [--vm V] [--timeout SECONDS] [--cwd GUESTDIR] -- PROGRAM ARGS...
       lab-run [--project P] [--vm V] status | reset | push HOSTPATH | pull GUESTPATH | sync REPO`

// wslFlag starts the arguments the WSL wrapper passes.
const wslFlag = "--wsl"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the exit, so the suite can call it.
func run(arguments []string, stdout, stderr io.Writer) int {
	wsl := "" // the caller's Linux working directory, when called from WSL
	if len(arguments) > 0 && arguments[0] == wslFlag {
		var err error
		if wsl, arguments, err = decodeWSL(arguments[1:]); err != nil {
			fmt.Fprintln(stderr, "lab-run: "+err.Error())
			return 64
		}
	}
	request, repo, why := parse(arguments)
	if why != "" {
		fmt.Fprintln(stderr, "lab-run: "+why)
		fmt.Fprintln(stderr, usage)
		return 64
	}
	projects, err := registry.Load(registryDirectory)
	if err != nil {
		fmt.Fprintln(stderr, "lab-run: "+err.Error())
		return 2
	}
	project, why := projectFor(projects, request.Project, wsl)
	if why != "" {
		fmt.Fprintln(stderr, "lab-run: "+why)
		return 2
	}
	request.Project = project.Name
	if request.Verb == lab.VerbPush {
		if request.HostPath, why = hostPath(project, request.HostPath, wsl); why != "" {
			fmt.Fprintln(stderr, "lab-run: push: "+why)
			return 2
		}
	}
	if repo != "" {
		if wsl != "" {
			return wslExit(sync(project, request, repo, wsl, stdout, stderr), stderr)
		}
		return sync(project, request, repo, wsl, stdout, stderr)
	}
	if wsl != "" {
		// From WSL, the lab exchange's paths in the broker's words (pull's new folder) in WSL's spelling.
		stdout = newSpellingWriter(stdout, project.Root, registry.ToWSL(project.Root))
		return wslExit(lab.Call(pipeName, brokerImage, request, stdout, stderr), stderr)
	}
	return lab.Call(pipeName, brokerImage, request, stdout, stderr)
}

// spellingWriter writes through, with each Windows path under the lab exchange rewritten in WSL's
// spelling (C:\Users\u\agent-lab\game\lab-out\X is /mnt/c/Users/u/agent-lab/game/lab-out/X). The
// broker sends each message whole, so a path is never split between writes.
type spellingWriter struct {
	out  io.Writer
	root *regexp.Regexp // the exchange's Windows path, any case, with the rest of the path after it
	wsl  string
}

// newSpellingWriter matches root on the text itself, case-insensitively: lowering the text first
// changed its byte offsets when it held a letter whose lower case is longer or shorter (İ, Ⱥ), which
// garbled the output or ran past its end (review 2026-11-21).
func newSpellingWriter(out io.Writer, root, wsl string) *spellingWriter {
	return &spellingWriter{out: out, root: regexp.MustCompile(`(?i)` + regexp.QuoteMeta(root) + `([^\s:*?"<>|]*)`), wsl: wsl}
}

func (w *spellingWriter) Write(p []byte) (int, error) {
	text := w.root.ReplaceAllStringFunc(string(p), func(match string) string {
		tail := w.root.FindStringSubmatch(match)[1]
		return w.wsl + strings.ReplaceAll(tail, `\`, "/")
	})
	if _, err := io.WriteString(w.out, text); err != nil {
		return 0, err
	}
	return len(p), nil
}

// wslExit is code as an exit status WSL keeps whole. A Linux exit status is 8 bits, and WSL passes
// on only the low byte of a Windows exit code, so PowerShell's -196608 (0xFFFD0000) read as 0,
// success, in WSL (game's live test, 2026-11-21). A code outside 0-255 is printed whole on stderr and
// becomes its low byte, or 1 when that byte is 0.
func wslExit(code int, stderr io.Writer) int {
	if code >= 0 && code <= 255 {
		return code
	}
	fmt.Fprintf(stderr, "lab-run: the guest program exited with %d (0x%08X)\n", code, uint32(code))
	if low := code & 0xFF; low != 0 {
		return low
	}
	return 1
}

// decodeWSL reads what the WSL wrapper passes: b64:CWD, then b64:ARG for each argument.
func decodeWSL(words []string) (string, []string, error) {
	if len(words) == 0 {
		return "", nil, errors.New(wslFlag + " needs the working directory")
	}
	decoded := make([]string, len(words))
	for i, word := range words {
		text, ok := strings.CutPrefix(word, "b64:")
		if !ok {
			return "", nil, fmt.Errorf("%s: argument %d is not b64:-encoded", wslFlag, i)
		}
		raw, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return "", nil, fmt.Errorf("%s: argument %d: %v", wslFlag, i, err)
		}
		decoded[i] = string(raw)
	}
	if !strings.HasPrefix(decoded[0], "/") {
		return "", nil, fmt.Errorf("%s: the working directory %q is not an absolute Linux path", wslFlag, decoded[0])
	}
	return decoded[0], decoded[1:], nil
}

// parse reads the options and the verb. repo is set for sync, which lab-run composes from push and run.
func parse(arguments []string) (lab.Request, string, string) {
	var request lab.Request
	i := 0
	for ; i < len(arguments); i++ {
		a := arguments[i]
		value := func() (string, bool) {
			if i+1 >= len(arguments) {
				return "", false
			}
			i++
			return arguments[i], true
		}
		var ok bool
		switch a {
		case "--project":
			request.Project, ok = value()
		case "--vm":
			request.VM, ok = value()
		case "--cwd":
			request.Cwd, ok = value()
		case "--timeout":
			var text string
			if text, ok = value(); ok {
				n, err := strconv.Atoi(text)
				if err != nil || n <= 0 {
					return request, "", "--timeout takes a whole number of seconds"
				}
				request.Timeout = n
			}
		case "--":
			request.Verb, request.Argv = lab.VerbRun, arguments[i+1:]
			if len(request.Argv) == 0 {
				return request, "", "nothing to run after --"
			}
			return request, "", ""
		default:
			if strings.HasPrefix(a, "-") {
				return request, "", "unknown option " + a
			}
			return verb(request, arguments[i:])
		}
		if !ok {
			return request, "", a + " needs a value"
		}
	}
	return request, "", "no verb"
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func verb(request lab.Request, words []string) (lab.Request, string, string) {
	name, rest := words[0], words[1:]
	switch {
	case (name == "status" || name == "reset") && len(rest) == 0:
		request.Verb = map[string]string{"status": lab.VerbStatus, "reset": lab.VerbReset}[name]
	case name == "push" && len(rest) == 1:
		request.Verb, request.HostPath = lab.VerbPush, rest[0]
	case name == "pull" && len(rest) == 1:
		request.Verb, request.Guest = lab.VerbPull, rest[0]
	case name == "sync" && len(rest) == 1:
		if !repoPattern.MatchString(rest[0]) || strings.HasSuffix(rest[0], ".bundle") {
			return request, "", "sync takes a repository's name (REPO, for lab-in/REPO.bundle)"
		}
		return request, rest[0], ""
	default:
		return request, "", fmt.Sprintf("%q is not a verb, or has the wrong number of arguments", strings.Join(words, " "))
	}
	return request, "", ""
}

// projectFor is the named project, or the one holding the working directory: from WSL, the one whose
// wsl_root holds it (or whose lab exchange does, for a /mnt/c/... directory).
func projectFor(projects []registry.Project, name, wsl string) (registry.Project, string) {
	if name != "" {
		for _, p := range projects {
			if p.Name == name {
				return p, ""
			}
		}
		return registry.Project{}, fmt.Sprintf("project %q is not registered", name)
	}
	if wsl != "" {
		if p, found := registry.ContainingWSL(projects, wsl); found {
			return p, ""
		}
		if windows, ok := registry.FromWSL(wsl); ok {
			if p, found := registry.Containing(projects, windows); found {
				return p, ""
			}
		}
		return registry.Project{}, wsl + " is in no registered project; name one with --project"
	}
	cwd, err := os.Getwd()
	if err != nil {
		return registry.Project{}, err.Error()
	}
	if p, found := registry.Containing(projects, cwd); found {
		return p, ""
	}
	return registry.Project{}, cwd + " is in no registered project; name one with --project"
}

// hostPath is a push's path as the absolute Windows path the broker reads. From WSL it must be on a
// Windows drive (/mnt/c/...); a path in the Linux file system gets the way to push it instead.
func hostPath(project registry.Project, given, wsl string) (string, string) {
	if wsl == "" || registry.IsAbsoluteWindows(given) {
		absolute, err := filepath.Abs(given)
		if err != nil {
			return "", err.Error()
		}
		return absolute, ""
	}
	linux := given
	if !path.IsAbs(linux) {
		linux = path.Join(wsl, linux)
	}
	if windows, ok := registry.FromWSL(linux); ok {
		return windows, ""
	}
	return "", fmt.Sprintf("%s is in the Linux file system, which the broker does not read: copy it into %s first (agents may write there), then push it from there",
		linux, registry.ToWSL(project.Abs(lab.LabIn)))
}

// sync pushes lab-in\REPO.bundle (a git bundle the agent made: git bundle create ... --all), clones
// it in the guest into a fresh directory, and only then replaces C:\work\REPO (review #9: a failed
// clone keeps the old checkout). No git runs on the host: making the bundle is the agent's, in its
// sandbox.
func sync(project registry.Project, request lab.Request, repo, wsl string, stdout, stderr io.Writer) int {
	bundle := filepath.Join(project.Abs(lab.LabIn), repo+".bundle")
	if info, err := os.Stat(bundle); err != nil || !info.Mode().IsRegular() {
		where := bundle
		if wsl != "" {
			where = registry.ToWSL(bundle)
		}
		fmt.Fprintf(stderr, "lab-run: sync: no bundle at %s; make it first: git -C REPO-DIRECTORY bundle create %s --all\n", where, where)
		return 2
	}
	push := request
	push.Verb, push.HostPath = lab.VerbPush, bundle
	if code := lab.Call(pipeName, brokerImage, push, stdout, stderr); code != 0 {
		return code
	}
	clone := request
	clone.Verb = lab.VerbRun
	clone.Argv = []string{"powershell", "-NoProfile", "-Command", syncScript(repo, transferID())}
	return lab.Call(pipeName, brokerImage, clone, stdout, stderr)
}

// syncScript clones C:\inbox\REPO.bundle into C:\work\.sync-ID\REPO, then swaps it into C:\work\REPO.
// repo and id are checked names ([A-Za-z0-9._-] and hex), so they are safe inside single quotes.
func syncScript(repo, id string) string {
	bundle := lab.GuestInbox + `\` + repo + ".bundle"
	fresh := lab.GuestWork + `\.sync-` + id + `\` + repo
	target := lab.GuestWork + `\` + repo
	old := lab.GuestWork + `\.old-` + id
	return "$ErrorActionPreference = 'Stop'; " +
		"git clone --quiet '" + bundle + "' '" + fresh + "'; " +
		"if (Test-Path '" + target + "') { Move-Item '" + target + "' '" + old + "' }; " +
		"Move-Item '" + fresh + "' '" + target + "'; " +
		"Remove-Item -Recurse -Force '" + old + "', '" + lab.GuestWork + `\.sync-` + id + "', '" + bundle + "' -ErrorAction SilentlyContinue; " +
		"git -C '" + target + "' log --oneline -1"
}

// transferID is 8 random hex digits.
func transferID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
