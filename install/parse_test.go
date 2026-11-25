// Package install holds no Go code, only this control: every PowerShell script in the repository must
// parse in Windows PowerShell 5.1, the PowerShell the owner runs. rebase-os.ps1 once shipped with
// "$name:" in a string, which 5.1 reads as a drive-qualified variable, and the suite passed (2026-11-23).
package install

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const windowsPowerShell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`

// parser prints one line per parse error and exits 1 if there was any.
const parser = `$bad = 0
foreach ($path in $args) {
  $tokens = $null; $errors = $null
  [System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$errors) | Out-Null
  foreach ($e in $errors) { Write-Output ('{0} line {1}: {2}' -f $path, $e.Extent.StartLineNumber, $e.Message); $bad++ }
}
if ($bad) { exit 1 }
Write-Output ('parsed ' + $args.Count)
`

func TestEveryScriptParsesInWindowsPowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 is on Windows only")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	var scripts []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".ps1") {
			scripts = append(scripts, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) == 0 || !containsSuffix(scripts, `install\rebase-os.ps1`) || !containsSuffix(scripts, `install\install.ps1`) {
		t.Fatalf("the walk found %q: not the repository's scripts", scripts)
	}
	checker := filepath.Join(t.TempDir(), "parse.ps1")
	if err := os.WriteFile(checker, []byte(parser), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(windowsPowerShell, append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", checker}, scripts...)...).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "parsed ") {
		t.Errorf("Windows PowerShell 5.1 does not parse every script (%v):\n%s", err, output)
	}
	// The control itself: a script with the 2026-11-23 mistake must fail.
	broken := filepath.Join(t.TempDir(), "broken.ps1")
	os.WriteFile(broken, []byte(`$x = 1; Write-Host "in $x: no"`), 0o644)
	if err := exec.Command(windowsPowerShell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", checker, broken).Run(); err == nil {
		t.Error(`the parser check passed a script with "$x:" in a string`)
	}
}

func containsSuffix(list []string, suffix string) bool {
	for _, item := range list {
		if strings.HasSuffix(strings.ToLower(item), strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}
