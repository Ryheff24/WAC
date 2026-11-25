package lab

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

// WindowsPowerShell is the PowerShell that ships with Windows, which has the Hyper-V module.
const WindowsPowerShell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`

// HyperV is the real Hypervisor: fixed Windows PowerShell scripts run by the broker as SYSTEM. A
// request's data reaches a script only through LAB_INPUT (base64 JSON), never through the script's
// text, so no request can inject PowerShell. The guest credential is decrypted inside the script
// (DPAPI, LocalMachine) and never leaves that process.
type HyperV struct {
	// States gives a VM's lab state, for the credential and the disks.
	States func(vm string) (State, error)
}

// prelude reads LAB_INPUT and defines Get-LabCredential and Wait-LabGuest.
const prelude = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$in = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:LAB_INPUT)) | ConvertFrom-Json
function Get-LabCredential {
  Add-Type -AssemblyName System.Security
  $bytes = [Security.Cryptography.ProtectedData]::Unprotect([IO.File]::ReadAllBytes($in.credential), $null, 'LocalMachine')
  $secure = ConvertTo-SecureString ([Text.Encoding]::UTF8.GetString($bytes)) -AsPlainText -Force
  [Array]::Clear($bytes, 0, $bytes.Length)
  New-Object Management.Automation.PSCredential($in.user, $secure)
}
function Wait-LabGuest {
  $deadline = (Get-Date).AddMinutes(5)
  $credential = Get-LabCredential
  while ($true) {
    try { Invoke-Command -VMName $in.vm -Credential $credential -ScriptBlock { 1 } | Out-Null; return } catch {
      if ((Get-Date) -gt $deadline) { throw "the guest did not answer PowerShell Direct within 5 minutes: $_" }
      Start-Sleep -Seconds 5
    }
  }
}
`

const infoScript = `$vm = Get-VM -Name $in.vm
$adapters = @(Get-VMNetworkAdapter -VMName $in.vm | ForEach-Object {
  $type = ''
  if ($_.SwitchName) { $type = "$((Get-VMSwitch -Name $_.SwitchName).SwitchType)" }
  [ordered]@{ switch = "$($_.SwitchName)"; switch_type = $type }
})
# By the service's Id, which is the same in every language (its Name is translated: review 2026-11-21, A4).
$gsi = [bool](@(Get-VMIntegrationService -VMName $in.vm | Where-Object { "$($_.Id)" -like '*6C09BB55-D683-4DA0-8931-C9BF705F6480' -and $_.Enabled }).Count)
[ordered]@{
  name = $vm.Name; state = "$($vm.State)"; adapters = $adapters; guest_service_interface = $gsi
  enhanced_session = [bool](Get-VMHost).EnableEnhancedSessionMode
  gpu_partitions = @(Get-VMGpuPartitionAdapter -VMName $in.vm -ErrorAction SilentlyContinue).Count
  disks = @(Get-VMHardDiskDrive -VMName $in.vm | ForEach-Object {
    $parent = ''
    if ($_.Path) { $parent = "$((Get-VHD -Path $_.Path).ParentPath)" }
    [ordered]@{ path = "$($_.Path)"; parent = $parent }
  })
  dvd_drives = @(Get-VMDvdDrive -VMName $in.vm).Count
  com_ports = @(Get-VMComPort -VMName $in.vm | Where-Object { $_.Path } | ForEach-Object { "$($_.Path)" })
  # A cmdlet this Windows lacks counts as none; any other error fails the read (review 2026-11-21, main 41dada9).
  assigned_devices = $(if (Get-Command Get-VMAssignableDevice -ErrorAction SilentlyContinue) { @(Get-VMAssignableDevice -VMName $in.vm).Count } else { 0 })
  fibre_channel = $(if (Get-Command Get-VMFibreChannelHba -ErrorAction SilentlyContinue) { @(Get-VMFibreChannelHba -VMName $in.vm).Count } else { 0 })
  # The least free on the volumes holding the attached disks' files (a path DriveInfo cannot read fails the read).
  disks_free = $(
    $least = [long]::MaxValue
    foreach ($drive in @(Get-VMHardDiskDrive -VMName $in.vm | Where-Object { $_.Path })) {
      $free = (New-Object IO.DriveInfo([IO.Path]::GetPathRoot($drive.Path))).AvailableFreeSpace
      if ($free -lt $least) { $least = $free }
    }
    $least)
} | ConvertTo-Json -Compress -Depth 4
`

const startScript = `Start-VM -Name $in.vm
Wait-LabGuest
`

const stopScript = `Stop-VM -Name $in.vm -TurnOff -Force
`

// probeScript runs in the guest: adapters up, TCP connects to each target (2 s each), a public DNS
// lookup, and the positive control: a loopback listener it connects to.
const probeScript = `$credential = Get-LabCredential
Invoke-Command -VMName $in.vm -Credential $credential -ArgumentList (,$in.targets) -ScriptBlock {
  param($targets)
  $result = [ordered]@{ hostname = $env:COMPUTERNAME; adapters_up = 0; reached = @(); dns = $false; loopback = $false }
  $result.adapters_up = @(Get-NetAdapter -ErrorAction SilentlyContinue | Where-Object Status -eq 'Up').Count
  foreach ($target in $targets) {
    $hostPart, $port = $target -split ':(?=\d+$)'
    $client = New-Object Net.Sockets.TcpClient
    try {
      $wait = $client.BeginConnect($hostPart.Trim('[', ']'), [int]$port, $null, $null)
      if ($wait.AsyncWaitHandle.WaitOne(2000) -and $client.Connected) { $result.reached += $target }
    } catch {} finally { $client.Close() }
  }
  try { [Net.Dns]::GetHostAddresses('example.com') | Out-Null; $result.dns = $true } catch {}
  $listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, 0)
  try {
    $listener.Start()
    $client = New-Object Net.Sockets.TcpClient
    $client.Connect([Net.IPAddress]::Loopback, $listener.LocalEndpoint.Port)
    $result.loopback = $client.Connected
    $client.Close()
  } catch {} finally { $listener.Stop() }
  $result | ConvertTo-Json -Compress
}
`

// labJobType is a guest-side type for Windows Job Objects: Join puts the calling process (and so
// every process it starts) in a new named job; Stop terminates a named job and all its processes.
const labJobType = `if (-not ('LabJob' -as [type])) { Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
public static class LabJob {
  [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)] static extern IntPtr CreateJobObject(IntPtr attributes, string name);
  [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)] static extern IntPtr OpenJobObject(uint access, bool inherit, string name);
  [DllImport("kernel32.dll", SetLastError = true)] static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);
  [DllImport("kernel32.dll", SetLastError = true)] static extern bool TerminateJobObject(IntPtr job, uint code);
  [DllImport("kernel32.dll")] static extern IntPtr GetCurrentProcess();
  [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
  static IntPtr held;
  public static void Join(string name) {
    IntPtr job = CreateJobObject(IntPtr.Zero, name);
    if (job == IntPtr.Zero) { throw new Win32Exception(); }
    if (!AssignProcessToJobObject(job, GetCurrentProcess())) { int e = Marshal.GetLastWin32Error(); CloseHandle(job); throw new Win32Exception(e); }
    held = job;
  }
  public static bool Stop(string name) {
    IntPtr job = OpenJobObject(0x0008, false, name);
    if (job == IntPtr.Zero) { return false; }
    try { return TerminateJobObject(job, 1); } finally { CloseHandle(job); }
  }
}
'@ }
`

// guestChild runs one program in the guest, in a child PowerShell of the run's own: it first joins the
// run's Job Object (LAB_JOB), so the program and everything it starts can be stopped together, and
// only them (runs share the VM). Its exit code is the program's. The run passes argv as base64 JSON
// (LAB_ARGV) and the program's command line (LAB_ARGS).
//
// A program (an .exe) gets the command line verbatim, after PowerShell's stop-parsing token: Windows
// PowerShell 5.1 does not escape quotes when it builds a native program's command line from separate
// arguments, so -Command '"a" + "b"' reached the guest as  a + b (2026-11-21, game's live test). A
// cmdlet, function or script gets argv as PowerShell arguments.
const guestChild = `$ErrorActionPreference = 'Continue'
$ProgressPreference = 'SilentlyContinue'
` + labJobType + `
try { [LabJob]::Join($env:LAB_JOB) } catch { [Console]::Error.WriteLine("lab-run (guest): this run's processes could not be put in a job, so a cancel cannot stop them: $($_.Exception.Message)") }
# Windows PowerShell 5.1's ConvertFrom-Json writes a JSON array as one object, and @( ) around it made a
# one-element list holding the whole array (the install's suite, 2026-11-21): enumerate it.
$argv = @(([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:LAB_ARGV)) | ConvertFrom-Json) | ForEach-Object { $_ })
if ($argv.Count -eq 0 -or $argv[0] -isnot [string]) { [Console]::Error.WriteLine('lab-run (guest): the run''s arguments did not arrive as a list of strings'); exit 1 }
$program = $argv[0]
$rest = @()
if ($argv.Count -gt 1) { $rest = $argv[1..($argv.Count - 1)] }
$found = @(Get-Command $program -ErrorAction SilentlyContinue)
$global:LASTEXITCODE = 0
if ($found[0].CommandType -eq 'Application') {
  if ($env:LAB_ARGS) {
    & $found[0].Source --% %LAB_ARGS%
  } else {
    & $found[0].Source
  }
} else {
  & $program @rest
}
$ok = $?
$code = $global:LASTEXITCODE
if ($null -eq $code -or ($code -eq 0 -and -not $ok)) { if ($ok) { $code = 0 } else { $code = 1 } }
exit $code
`

// guestRun is the guest side of a run: a script block's body, given argv, the working directory,
// line (the command line of argv[1:], from commandLine), marker (this run's exit marker), child
// (guestChild, encoded) and job (the run's Job Object name). Guest stderr is merged into stdout (2>&1).
// The output always ends with "MARKER CODE", even when the program cannot start or the working
// directory does not exist (then a "lab-run (guest):" line says why and the code is 1). The marker is
// random per run, so no output line can pose as it.
const guestRun = `param($argv, $cwd, $line, $marker, $child, $job)
  $ErrorActionPreference = 'Continue'
  # The guest session's own progress records ("Preparing modules for first use") reached the caller as
  # raw CLIXML on every run (tested 2026-11-21).
  $ProgressPreference = 'SilentlyContinue'
  $code = 1
  try {
    if ($cwd) { Set-Location -LiteralPath $cwd -ErrorAction Stop }
    $program = $argv[0]
    $found = @(Get-Command $program -ErrorAction SilentlyContinue)
    if ($found.Count -eq 0) { throw "there is no program or command named $program in the guest" }
    $env:LAB_ARGS = $line
    $env:LAB_ARGV = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes((ConvertTo-Json -InputObject @($argv) -Compress)))
    $env:LAB_JOB = $job
    $global:LASTEXITCODE = 0
    & "$PSHOME\powershell.exe" -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand $child 2>&1 | ForEach-Object { "$_" }
    $code = $global:LASTEXITCODE
    if ($null -eq $code) { $code = 1 }
  } catch {
    "lab-run (guest): $($_.Exception.Message)"
    $code = 1
  } finally {
    "$marker $code"
  }
`

// guestStop is the guest side of stopping a run: terminate its Job Object, every process in it.
const guestStop = `param($job)
` + labJobType + `
if ([LabJob]::Stop($job)) { 'stopped' } else { 'none' }
`

// runScript runs argv in the guest (guestRun over PowerShell Direct), streaming each output line as it
// comes. Errors the guest session reports arrive as lines too (-ErrorAction Continue, 2>&1): under
// this script's Stop they ended the call before the exit marker (2026-11-21, a bad -File).
const runScript = `$credential = Get-LabCredential
Invoke-Command -VMName $in.vm -Credential $credential -ErrorAction Continue -ArgumentList $in.argv, $in.cwd, $in.line, $in.marker, $in.child, $in.job -ScriptBlock {
` + guestRun + `} 2>&1 | ForEach-Object { [Console]::Out.WriteLine("$_"); [Console]::Out.Flush() }
`

// stopRunScript stops a run's guest processes from a second PowerShell Direct session.
const stopRunScript = `Invoke-Command -VMName $in.vm -Credential (Get-LabCredential) -ArgumentList $in.job -ScriptBlock {
` + guestStop + `}
`

// commandLine is argv as one Windows command line that CommandLineToArgvW (and every program built
// with the usual C runtime, Go and .NET included) splits back into exactly argv: quotes, backslashes
// before quotes, trailing backslashes, spaces and empty arguments included.
func commandLine(argv []string) string {
	quoted := make([]string, len(argv))
	for i, argument := range argv {
		quoted[i] = syscall.EscapeArg(argument)
	}
	return strings.Join(quoted, " ")
}

const copyInScript = `$session = New-PSSession -VMName $in.vm -Credential (Get-LabCredential)
try {
  Invoke-Command -Session $session -ArgumentList $in.guest -ScriptBlock { param($d) New-Item -ItemType Directory -Force -Path $d | Out-Null }
  Copy-Item -ToSession $session -LiteralPath $in.host -Destination $in.guest -Recurse -Force
} finally { Remove-PSSession $session }
`

// copyOutScript pulls a guest path as one tar archive. In the guest: refuse links (junctions and
// symbolic links, at any depth) and a tree over max bytes, then pack it with Windows' tar.exe. On the
// host: read the archive in chunks into the new file $in.host and stop at max bytes. The guest is the
// untrusted side, so nothing it names or sizes is taken on its word: the host's file is the broker's
// own name, its size is counted here, and lab.extractArchive unpacks it with its own checks.
const copyOutScript = `$session = New-PSSession -VMName $in.vm -Credential (Get-LabCredential)
try {
  $archive = Invoke-Command -Session $session -ArgumentList $in.guest, $in.max -ScriptBlock {
    param($path, $max)
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    $item = Get-Item -LiteralPath $path -Force
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "$path is a link or junction: pull what it points to" }
    if ($item.PSIsContainer) {
      $links = @(Get-ChildItem -LiteralPath $path -Recurse -Force -Attributes ReparsePoint -ErrorAction SilentlyContinue)
      if ($links.Count) { throw "$path holds $($links.Count) links or junctions (first: $($links[0].FullName)); pull a tree without them" }
      $all = @(Get-ChildItem -LiteralPath $path -Recurse -Force)
      $size = ($all | Where-Object { -not $_.PSIsContainer } | Measure-Object -Property Length -Sum).Sum
      $entries = $all.Count + 1
    } else { $size = $item.Length; $entries = 1 }
    # The archive adds about a kilobyte per entry (headers and padding) to the files' bytes.
    if ($size + 1024 * ($entries + 2) -gt $max) { throw "$path is $size bytes in $entries entries; a pull takes at most $max bytes now" }
    $archive = Join-Path $env:TEMP ('lab-pull-' + [guid]::NewGuid().ToString('N') + '.tar')
    & "$env:SystemRoot\System32\tar.exe" -c -f $archive -C (Split-Path -LiteralPath $item.FullName) ('./' + $item.Name) | Out-Null
    if ($LASTEXITCODE) { throw "tar.exe failed (exit $LASTEXITCODE)" }
    $archive
  }
  try {
  $out = [IO.File]::Open($in.host, 'CreateNew', 'Write')
  try {
    $offset = [long]0
    while ($true) {
      $chunk = Invoke-Command -Session $session -ArgumentList $archive, $offset -ScriptBlock {
        param($file, $offset)
        $stream = [IO.File]::OpenRead($file)
        try {
          $null = $stream.Seek($offset, 'Begin')
          $buffer = New-Object byte[] 4194304
          $n = $stream.Read($buffer, 0, $buffer.Length)
          [Array]::Resize([ref]$buffer, $n)
          ,$buffer
        } finally { $stream.Close() }
      }
      if ($chunk -isnot [byte[]]) { throw 'the guest sent something other than the archive' }
      if ($chunk.Length -eq 0) { break }
      if ($offset + $chunk.Length -gt $in.max) { throw "the guest's archive passed $($in.max) bytes" }
      $out.Write($chunk, 0, $chunk.Length)
      $offset += $chunk.Length
    }
  } finally { $out.Close() }
  } finally {
    # The guest's archive goes whether the copy finished or not.
    Invoke-Command -Session $session -ArgumentList $archive -ScriptBlock { param($f) Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue }
  }
} finally { Remove-PSSession $session }
`

// resetScript turns the VM off, recreates each differencing disk from its parent, and starts it.
const resetScript = `if ((Get-VM -Name $in.vm).State -ne 'Off') { Stop-VM -Name $in.vm -TurnOff -Force }
# The VM's own account must be able to open each new child. Hyper-V grants it only when a disk is
# attached, and these are swapped in under paths already attached: without the grant, Start-VM said
# "Account does not have permission to open attachment ... Access is denied" (2026-11-23).
$account = 'NT VIRTUAL MACHINE\' + (Get-VM -Name $in.vm).Id
foreach ($pair in $in.diffs) {
  # The new child is made beside the old one and only then swapped in, so a reset cut short never
  # leaves the VM without its recorded disk (review 2026-11-21, A1).
  $fresh = $pair.diff + '.new.vhdx'
  if (Test-Path -LiteralPath $fresh) { Remove-Item -LiteralPath $fresh -Force }
  New-VHD -Path $fresh -ParentPath $pair.parent -Differencing | Out-Null
  icacls $fresh /grant "$($account):(F)" /Q | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "icacls could not grant $account on $fresh (exit $LASTEXITCODE)" }
  if (Test-Path -LiteralPath $pair.diff) { Remove-Item -LiteralPath $pair.diff -Force }
  Move-Item -LiteralPath $fresh -Destination $pair.diff
}
Start-VM -Name $in.vm
Wait-LabGuest
`

func (h HyperV) input(vm string, fields map[string]any) (map[string]any, error) {
	state, err := h.States(vm)
	if err != nil {
		return nil, err
	}
	input := map[string]any{"vm": vm, "credential": state.Credential, "user": state.GuestUser}
	for key, value := range fields {
		input[key] = value
	}
	return input, nil
}

func (h HyperV) Info(ctx context.Context, vm string) (VMInfo, error) {
	var out bytes.Buffer
	if err := powershell(ctx, infoScript, map[string]any{"vm": vm}, &out, nil); err != nil {
		return VMInfo{}, err
	}
	return parseInfo(out.String())
}

func (h HyperV) Start(ctx context.Context, vm string) error {
	input, err := h.input(vm, nil)
	if err != nil {
		return err
	}
	return powershell(ctx, startScript, input, io.Discard, nil)
}

func (h HyperV) Stop(ctx context.Context, vm string) error {
	return powershell(ctx, stopScript, map[string]any{"vm": vm}, io.Discard, nil)
}

func (h HyperV) Probe(ctx context.Context, vm string, targets []string) (ProbeResult, error) {
	input, err := h.input(vm, map[string]any{"targets": targets})
	if err != nil {
		return ProbeResult{}, err
	}
	var out bytes.Buffer
	if err := powershell(ctx, probeScript, input, &out, nil); err != nil {
		return ProbeResult{}, err
	}
	return parseProbe(out.String())
}

func (h HyperV) Run(ctx context.Context, vm string, argv []string, cwd string, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("no program given")
	}
	marker := newMarker()
	job := `Global\lab-run-` + marker
	input, err := h.input(vm, map[string]any{"argv": argv, "cwd": cwd, "line": commandLine(argv[1:]), "marker": marker,
		"child": encodeCommand(guestChild), "job": job})
	if err != nil {
		return 0, err
	}
	reader, writer := io.Pipe()
	type relayed struct {
		code  int
		found bool
	}
	result := make(chan relayed, 1)
	go func() { code, found := relayRun(reader, stdout, marker); result <- relayed{code, found} }()
	err = powershell(ctx, runScript, input, writer, stderr)
	writer.Close()
	got := <-result
	if ctx.Err() != nil {
		// Cut short (the client went away, the timeout, a reset, a failed recheck): ending the host's
		// PowerShell does not end the guest's, so stop what this run started there (game, 2026-11-21:
		// a cancelled extraction ran on for 30 minutes).
		return 0, fmt.Errorf("%w; %s", ctx.Err(), h.stopRun(ctx, vm, job))
	}
	return runResult(got.code, got.found, err)
}

// stopRun terminates a run's Job Object in the guest, on its own two minutes (the run's context is
// already done), and says how that went.
func (h HyperV) stopRun(ctx context.Context, vm, job string) string {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	input, err := h.input(vm, map[string]any{"job": job})
	if err != nil {
		return "the guest's processes were not stopped: " + err.Error()
	}
	var out bytes.Buffer
	if err := powershell(stopCtx, stopRunScript, input, &out, nil); err != nil {
		return "stopping the guest's processes failed: " + err.Error()
	}
	if strings.Contains(out.String(), "stopped") {
		return "the guest's processes were stopped"
	}
	return "no guest processes were left"
}

// runResult is a run's exit code from what relayRun found and how PowerShell ended. Any code the
// marker carried is the program's, negative ones included (-196608 for powershell -File of a missing
// file, an NTSTATUS on a crash): only a missing marker is a failure.
func runResult(code int, found bool, err error) (int, error) {
	if !found {
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("the guest run ended without an exit code")
	}
	return code, nil
}

// newMarker is a run's exit marker: random, so nothing the guest prints can pose as it.
func newMarker() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "__LAB_EXIT_" + hex.EncodeToString(b) + "__"
}

// relayRun copies lines to stdout until the exit marker, and returns its code and whether it came.
// A line that only looks like a marker is output like any other.
func relayRun(reader io.Reader, stdout io.Writer, marker string) (code int, found bool) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if rest, isMarker := strings.CutPrefix(line, marker+" "); isMarker {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				code, found = n, true
				continue
			}
		}
		io.WriteString(stdout, line+"\n")
	}
	io.Copy(io.Discard, reader)
	return code, found
}

func (h HyperV) CopyIn(ctx context.Context, vm string, hostPath, guestDirectory string) error {
	input, err := h.input(vm, map[string]any{"host": hostPath, "guest": guestDirectory})
	if err != nil {
		return err
	}
	return powershell(ctx, copyInScript, input, io.Discard, nil)
}

func (h HyperV) CopyOut(ctx context.Context, vm string, guestPath, hostArchive string, max int64) error {
	input, err := h.input(vm, map[string]any{"guest": guestPath, "host": hostArchive, "max": max})
	if err != nil {
		return err
	}
	return powershell(ctx, copyOutScript, input, io.Discard, nil)
}

func (h HyperV) Reset(ctx context.Context, vm string, state State) error {
	diffs := []map[string]string{{"parent": state.OSBase, "diff": state.OSDiff}}
	if state.GameParent != nil {
		diffs = append(diffs, map[string]string{"parent": state.GameParent.Path, "diff": state.GameDiff})
	}
	input, err := h.input(vm, map[string]any{"diffs": diffs})
	if err != nil {
		return err
	}
	// Not cut short by the client going away or another reset: a reset half done is worse than one
	// finished late. It has its own bound instead.
	resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()
	return powershell(resetCtx, resetScript, input, io.Discard, nil)
}

// powershell runs prelude+script with input as LAB_INPUT. stderr nil collects it into the error.
func powershell(ctx context.Context, script string, input map[string]any, stdout, stderr io.Writer) error {
	encoded, err := encodeInput(input)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, WindowsPowerShell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeCommand(prelude+script))
	command.Env = append(command.Environ(), "LAB_INPUT="+encoded)
	var collected bytes.Buffer
	command.Stdout = stdout
	if stderr != nil {
		command.Stderr = stderr
	} else {
		command.Stderr = &collected
	}
	if err := command.Run(); err != nil {
		if text := strings.TrimSpace(collected.String()); text != "" {
			return fmt.Errorf("%w: %s", err, oneLine(text))
		}
		return err
	}
	return nil
}

// encodeCommand is PowerShell's -EncodedCommand form: base64 of UTF-16LE.
func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 2*len(units))
	for i, unit := range units {
		raw[2*i] = byte(unit)
		raw[2*i+1] = byte(unit >> 8)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// encodeInput is the LAB_INPUT value: base64 of the JSON.
func encodeInput(input map[string]any) (string, error) {
	text, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(text), nil
}

// parseInfo reads infoScript's JSON (the last non-empty line).
func parseInfo(output string) (VMInfo, error) {
	var info VMInfo
	if err := json.Unmarshal([]byte(lastLine(output)), &info); err != nil {
		return VMInfo{}, fmt.Errorf("unreadable VM info %q: %w", oneLine(output), err)
	}
	return info, nil
}

// parseProbe reads probeScript's JSON (the last non-empty line).
func parseProbe(output string) (ProbeResult, error) {
	var result ProbeResult
	if err := json.Unmarshal([]byte(lastLine(output)), &result); err != nil {
		return ProbeResult{}, fmt.Errorf("unreadable probe result %q: %w", oneLine(output), err)
	}
	return result, nil
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// oneLine is text on one line. PowerShell's serialized error stream (#< CLIXML ... <S S="Error">...)
// is reduced to its messages first (review 2026-11-21, #8).
func oneLine(text string) string {
	if strings.HasPrefix(strings.TrimSpace(text), "#< CLIXML") {
		var messages []string
		for _, m := range clixmlError.FindAllStringSubmatch(text, -1) {
			message := strings.NewReplacer("_x000D_", "", "_x000A_", " ", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&").Replace(m[1])
			messages = append(messages, message)
		}
		if len(messages) > 0 {
			text = strings.Join(messages, " ")
		}
	}
	// A PowerShell error record as text: its message, then "At line:N char:M", the line, and
	// "+ CategoryInfo ... + FullyQualifiedErrorId": only the message is the reason.
	if i := strings.Index(text, "At line:"); i > 0 {
		text = text[:i]
	}
	if i := strings.Index(text, "+ CategoryInfo"); i > 0 {
		text = text[:i]
	}
	return strings.Join(strings.Fields(text), " ")
}

var clixmlError = regexp.MustCompile(`<S S="Error">([^<]*)</S>`)
