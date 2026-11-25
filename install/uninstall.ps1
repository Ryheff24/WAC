#Requires -RunAsAdministrator
# uninstall.ps1: removes what install.ps1 installed, and nothing else. From an elevated PowerShell:
#   powershell -NoProfile -ExecutionPolicy Bypass -File install\uninstall.ps1
#
# Removed: the scheduled task \agent-containment\lab-broker (stopped first), lab-broker.exe,
# lab-run.exe, rebase-os.ps1 and installed-commit.txt in C:\Program Files\agent-containment (the folder too, once
# empty), and in C:\ProgramData\agent-containment: projects\ (the registry), src\ (the build clones),
# toolcache\ and staging\.
#
# Kept: every lab VM and its disks (with the old system disks a rebase set aside), the game disk, labs\ (the lab states) and the copies install.ps1
# made of them before changing them (labs.before-<time>\), secrets\, vms\, media\,
# log\ (the broker's record of every call), each project's lab exchange (what was pulled is yours),
# and the host's Enhanced Session mode off (turn it back on yourself if you want it:
# Set-VMHost -EnableEnhancedSessionMode $true). The WSL side has its own uninstall:
# sudo bash wsl/uninstall.sh.
# Exit 0 when everything listed as removed is gone, 1 otherwise.

$ErrorActionPreference = 'Stop'
$Programs = 'C:\Program Files\agent-containment'
$Data = 'C:\ProgramData\agent-containment'
$TaskPath = '\agent-containment\'
$TaskName = 'lab-broker'

$task = Get-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -ErrorAction SilentlyContinue
if ($task) {
    Write-Host "Removing the task $TaskPath$TaskName"
    Stop-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -Confirm:$false
}
# The task's folder too, once empty (it fails, harmlessly, while anything else is in it).
try {
    $scheduler = New-Object -ComObject Schedule.Service
    $scheduler.Connect()
    $scheduler.GetFolder('\').DeleteFolder($TaskPath.Trim('\'), 0)
} catch { }
# The broker outlives Stop-ScheduledTask for a moment; then it is stopped outright.
$broker = "$Programs\lab-broker.exe"
$deadline = (Get-Date).AddSeconds(30)
while ((Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -ieq $broker }) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -ieq $broker } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

$removed = @("$Programs\lab-broker.exe", "$Programs\lab-run.exe", "$Programs\rebase-os.ps1", "$Programs\installed-commit.txt") +
    @(Get-ChildItem -LiteralPath $Programs -Filter '*.old-*' -ErrorAction SilentlyContinue | ForEach-Object { $_.FullName }) +
    @("$Data\projects", "$Data\projects.incoming", "$Data\src", "$Data\toolcache", "$Data\staging")
foreach ($path in $removed) {
    if (Test-Path -LiteralPath $path) {
        # Never through a link or junction: removing it would remove what it points to (review 2026-11-21, A10).
        if ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) {
            Write-Host "Not removing $path`: it is a link or junction; remove it yourself" -ForegroundColor Yellow
            continue
        }
        Write-Host "Removing $path"
        Remove-Item -LiteralPath $path -Recurse -Force -ErrorAction Continue
    }
}
if ((Test-Path -LiteralPath $Programs) -and -not (Get-ChildItem -LiteralPath $Programs -Force)) {
    Remove-Item -LiteralPath $Programs -Force
}

$left = @($removed | Where-Object { Test-Path -LiteralPath $_ })
if (Get-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -ErrorAction SilentlyContinue) { $left += "task $TaskPath$TaskName" }
if ($left) {
    foreach ($path in $left) { Write-Host "STILL THERE: $path (a lab-run.exe still running holds it: close it, then run this again)" -ForegroundColor Red }
    exit 1
}
Write-Host 'Uninstalled. Kept: the lab VMs and their disks, and in C:\ProgramData\agent-containment labs\, secrets\, vms\, media\ and log\. Enhanced Session mode stays off.' -ForegroundColor Green
exit 0
