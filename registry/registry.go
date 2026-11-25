// Package registry reads the project registry: one JSON file per project in the owner's
// admin-only directory (Directory), installed from main by install\install.ps1. Nothing else in this
// module names a project.
//
// A project here is only what the lab needs: its name, its lab VMs, and the Windows folder lab
// transfers go through (Root: push reads from it, pull writes into its lab-out). The project itself
// lives in WSL (WSLRoot); lab-run uses that to tell which project a WSL working directory is in.
//
// A file that cannot be read, has an unknown field, or fails Validate refuses the whole registry
// (fail closed).
package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Directory is where the owner's install puts the registry. Administrators and SYSTEM may write
// it; everyone else may only read it.
const Directory = `C:\ProgramData\agent-containment\projects`

// Project is one project's entry.
type Project struct {
	// Name is the file's stem and the project's handle in lab-run --project.
	Name string `json:"name"`
	// Root is the project's lab exchange on Windows, absolute: push reads only from inside it, pull
	// publishes into its lab-out. Agents write only its lab-in (the WSL sandbox's allowWrite).
	Root string `json:"root"`
	// WSLRoot is the project's directory in WSL, absolute; empty for a project without one.
	WSLRoot string `json:"wsl_root,omitempty"`
	// LabVMs are the Hyper-V VMs this project's code may run in. The first is the default.
	LabVMs []string `json:"lab_vms"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var vmPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// Validate reports the first problem with p, or nil.
func (p Project) Validate() error {
	if !namePattern.MatchString(p.Name) {
		return fmt.Errorf("name %q: use 1-32 lower-case letters, digits and '-'", p.Name)
	}
	if !IsAbsoluteWindows(p.Root) || strings.Contains(p.Root, "..") {
		return fmt.Errorf("%s: root %q is not an absolute Windows path (C:\\...)", p.Name, p.Root)
	}
	if p.WSLRoot != "" && (!strings.HasPrefix(p.WSLRoot, "/") || path.Clean(p.WSLRoot) != p.WSLRoot || p.WSLRoot == "/") {
		return fmt.Errorf("%s: wsl_root %q is not a clean absolute Linux path below /", p.Name, p.WSLRoot)
	}
	if len(p.LabVMs) == 0 {
		return fmt.Errorf("%s: lab_vms is empty; every project names at least one lab VM", p.Name)
	}
	for _, vm := range p.LabVMs {
		if !vmPattern.MatchString(vm) {
			return fmt.Errorf("%s: lab VM name %q: use letters, digits and '-'", p.Name, vm)
		}
	}
	return nil
}

// Abs is path made absolute against Root and cleaned.
func (p Project) Abs(path string) string {
	if IsAbsoluteWindows(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(p.Root, path))
}

// Load reads every *.json in directory, strictly, and validates the set. Names must match the
// file stem and be unique, and no project's root (or WSL root) may contain another's.
func Load(directory string) ([]Project, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", directory, err)
	}
	var projects []Project
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		text, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", path, err)
		}
		project, err := Parse(text)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", path, err)
		}
		stem := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if project.Name != stem {
			return nil, fmt.Errorf("registry %s: name %q does not match the file name", path, project.Name)
		}
		projects = append(projects, project)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("registry %s: no project files", directory)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	for i := range projects {
		for j := range projects {
			if i == j {
				continue
			}
			if Within(projects[i].Root, projects[j].Root) {
				return nil, fmt.Errorf("registry: %s's root %s is inside %s's root %s; project roots may not nest",
					projects[j].Name, projects[j].Root, projects[i].Name, projects[i].Root)
			}
			if projects[i].WSLRoot != "" && WithinLinux(projects[i].WSLRoot, projects[j].WSLRoot) {
				return nil, fmt.Errorf("registry: %s's wsl_root is inside %s's; project roots may not nest", projects[j].Name, projects[i].Name)
			}
		}
	}
	return projects, nil
}

// Parse decodes one project file, refusing unknown fields and trailing data, and validates it.
func Parse(text []byte) (Project, error) {
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	var project Project
	if err := decoder.Decode(&project); err != nil {
		return Project{}, err
	}
	if decoder.More() {
		return Project{}, errors.New("trailing data after the project object")
	}
	// Validated as written, then cleaned: on Windows, cleaning would turn C:\x\..\y into C:\y and hide
	// the ".." Validate refuses (CI on #16).
	if err := project.Validate(); err != nil {
		return Project{}, err
	}
	project.Root = filepath.Clean(project.Root)
	return project, nil
}

// Containing returns the project whose root holds path, if any.
func Containing(projects []Project, path string) (Project, bool) {
	for _, project := range projects {
		if Within(project.Root, path) {
			return project, true
		}
	}
	return Project{}, false
}

// ContainingWSL returns the project whose WSL root holds the Linux path, if any.
func ContainingWSL(projects []Project, linuxPath string) (Project, bool) {
	for _, project := range projects {
		if project.WSLRoot != "" && WithinLinux(project.WSLRoot, linuxPath) {
			return project, true
		}
	}
	return Project{}, false
}

// Within reports whether inner is outer or below it. Windows paths compare without case, after
// cleaning; a path in Git Bash's /c/... spelling is converted first. Lexical, not by file identity:
// the broker refuses any transfer through a link or junction (lab/transfer.go).
func Within(outer, inner string) bool {
	outer, inner = Normalize(outer), Normalize(inner)
	if outer == "" || inner == "" {
		return false
	}
	if strings.EqualFold(outer, inner) {
		return true
	}
	prefix := outer
	if !strings.HasSuffix(prefix, `\`) {
		prefix += `\`
	}
	return len(inner) > len(prefix) && strings.EqualFold(inner[:len(prefix)], prefix)
}

// WithinLinux reports whether the Linux path inner is outer or below it, after cleaning, with case.
func WithinLinux(outer, inner string) bool {
	if !strings.HasPrefix(outer, "/") || !strings.HasPrefix(inner, "/") {
		return false
	}
	outer, inner = path.Clean(outer), path.Clean(inner)
	return inner == outer || strings.HasPrefix(inner, strings.TrimSuffix(outer, "/")+"/")
}

var driveSlash = regexp.MustCompile(`^/([A-Za-z])(/|$)`)

// Normalize turns a Windows path in any of its spellings (C:\a, C:/a, /c/a as Git Bash writes it,
// \\?\C:\a) into a cleaned C:\a form. A path it cannot read as absolute is returned cleaned but
// otherwise unchanged.
func Normalize(path string) string {
	path = strings.TrimPrefix(path, `\\?\`)
	if m := driveSlash.FindStringSubmatch(path); m != nil {
		path = strings.ToUpper(m[1]) + `:\` + path[len(m[0]):]
	}
	path = strings.ReplaceAll(path, "/", `\`)
	if len(path) >= 2 && path[1] == ':' {
		path = strings.ToUpper(path[:1]) + path[1:]
	}
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

// IsAbsoluteWindows reports whether path starts with a drive letter, a colon and a slash.
func IsAbsoluteWindows(path string) bool {
	return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') &&
		(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z')
}

var mountPattern = regexp.MustCompile(`^/mnt/([a-z])(/.*)?$`)

// FromWSL turns a Linux path on a Windows drive (/mnt/c/a/b, WSL's default automount) into the
// Windows path (C:\a\b). ok is false for any other path: the Linux file system (/home/...) is not
// one the broker reads.
//
// A backslash or colon is refused: legal in a Linux name, each would mean something else in the
// Windows path (lab-in/..\..\x).
func FromWSL(linuxPath string) (string, bool) {
	if !strings.HasPrefix(linuxPath, "/") || strings.ContainsAny(linuxPath, `\:`) {
		return "", false
	}
	m := mountPattern.FindStringSubmatch(path.Clean(linuxPath))
	if m == nil {
		return "", false
	}
	return Normalize(strings.ToUpper(m[1]) + `:\` + strings.ReplaceAll(strings.TrimPrefix(m[2], "/"), "/", `\`)), true
}

// ToWSL is the WSL spelling of a Windows drive path (C:\a\b is /mnt/c/a/b), for messages.
func ToWSL(windowsPath string) string {
	p := Normalize(windowsPath)
	if !IsAbsoluteWindows(p) {
		return p
	}
	rest := strings.Trim(strings.ReplaceAll(p[2:], `\`, "/"), "/")
	if rest == "" {
		return "/mnt/" + strings.ToLower(p[:1])
	}
	return "/mnt/" + strings.ToLower(p[:1]) + "/" + rest
}
