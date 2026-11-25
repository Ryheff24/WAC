# rebase-os.ps1: gives a lab VM a fresh Windows Evaluation system disk, activated online once while it
# is the only disk the VM has, then sealed as the VM's base. The VM keeps its other disks (the game
# chain, the work disk), its lab state and its guest account. Windows Enterprise Evaluation stops
# working about 90 days after activation, so this is repeated every ~90 days.
#
# Run the INSTALLED copy (install.ps1 puts it in C:\Program Files\agent-containment, where only
# administrators write), in one line; it asks for elevation once:
#   powershell -NoProfile -ExecutionPolicy Bypass -File "C:\Program Files\agent-containment\rebase-os.ps1" -VM game-lab
#
# The owner's rule, asserted at the start and end of every phase and before and after every change of
# adapter: the VM never has a network adapter while any disk but the new, blank system disk is
# attached (the game disk and the work disk, which may hold a quarantined sample, never meet a network).
#
# The phases (resumable: run the same line again and it continues where it stopped):
#   1 prepare   the broker task disabled (nothing else touches the VM); the VM shut down; its disks
#               checked against the lab state, recorded (path, controller, location) and detached; the
#               old os_base and os_diff moved aside into <vm folder>\rebase\ (kept for -Rollback)
#   2 build     a new system disk at the os_base path, with Windows applied offline from the ISO (DISM,
#               as the lab was first built: no Windows Setup, no DVD drive), an answer file that names the
#               guest as the lab state does and creates guest_user with the password in the existing
#               credential blob (secrets\ does not change), and the host's NVIDIA driver files for the
#               VM's GPU partition
#   3 activate  the new disk alone in the VM; first boot; then the Default Switch adapter, slmgr /ato,
#               and the adapter removed again (also on any failure); the license checked offline;
#               guest_user's password set never to expire; drive optimisation (the Defrag tasks and
#               defragsvc) and last-access writes off (they rewrote the game volume), checked again
#               after a reboot; shut down
#   4 finish    the new disk read-only (the new os_base), a new os_diff over it, every recorded disk
#               reattached where it was; the broker task enabled; lab-broker check, lab-run status,
#               and a run through the gate (which starts the VM and probes it); the activation's end
#               printed
# The lab state file is never written: the new disks take the old paths.
#
# The ISO: by default the one ISO in C:\ProgramData\agent-containment\media (the lab's pinned copy, named
# by its SHA-256). Another one: -Iso PATH -IsoSha256 HEX; it is copied into media\ and that copy is
# hashed and used. Its setup.exe must carry Microsoft's signature.
#
# Other modes:
#   -Rollback  puts the old os_base and os_diff back (deleting the new ones) and reattaches the
#              recorded disks; at any phase, also after finish, until -Forget
#   -Seal      folds os_diff into os_base (as the old seal-lab.ps1 did), so a reset returns to the
#              guest as it is now (tools installed). Only from a guest that has run nothing untrusted
#              since its last reset: whatever is in os_diff becomes part of every reset
#   -Forget    deletes the old disks a finished rebase kept; the next rebase needs this first
#
# Windows PowerShell 5.1 or PowerShell 7. Any failure stops it with a FAILED line and exit 1.

param(
    [Parameter(Mandatory)][string]$VM,
    [string]$Iso = '',
    [string]$IsoSha256 = '',
    [switch]$Rollback,
    [switch]$Seal,
    [switch]$Forget
)

$ErrorActionPreference = 'Stop'
$BrokerStopped = $false
$RebaseDir = ''
$PhaseFile = ''
$ProgressPreference = 'SilentlyContinue'
$Programs = 'C:\Program Files\agent-containment'
$Data = 'C:\ProgramData\agent-containment'
$TaskPath = '\agent-containment\'
$TaskName = 'lab-broker'
$Pipe = 'agent-containment-lab'
$DefaultSwitchId = 'c08cb7b8-9b3c-408e-8e30-5e16a3aeb444'
# Windows' own licensing application (SoftwareLicensingProduct.ApplicationID).
$WindowsApplication = '55c92734-d682-4d71-983e-d6ec3f16059f'

function Format-Quoted([string]$Text) { "'" + $Text.Replace("'", "''") + "'" }

# Elevated code runs only from where install.ps1 put it, built from a fresh clone of main.
if (-not $PSCommandPath -or -not $PSCommandPath.StartsWith("$Programs\", [StringComparison]::OrdinalIgnoreCase)) {
    Write-Host "FAILED: run the installed copy, $Programs\rebase-os.ps1 (install.ps1 puts it there)" -ForegroundColor Red
    exit 1
}
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    $command = "& $(Format-Quoted $PSCommandPath) -VM $(Format-Quoted $VM)"
    if ($Iso) { $command += " -Iso $(Format-Quoted $Iso)" }
    if ($IsoSha256) { $command += " -IsoSha256 $(Format-Quoted $IsoSha256)" }
    if ($Rollback) { $command += ' -Rollback' }
    if ($Seal) { $command += ' -Seal' }
    if ($Forget) { $command += ' -Forget' }
    $command += "; `$code = `$LASTEXITCODE; Read-Host ('rebase-os.ps1 finished (exit ' + `$code + '). Press Enter to close this window'); exit `$code"
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
    Write-Host 'Asking for elevation (one UAC prompt); the work happens in the elevated window.'
    $process = Start-Process powershell.exe -Verb RunAs -Wait -PassThru -ArgumentList "-NoProfile -ExecutionPolicy Bypass -EncodedCommand $encoded"
    exit $process.ExitCode
}

# Invoke-Checked runs a native program and throws on a non-zero exit. Its parameters have names no
# caller uses: PowerShell scopes variables dynamically and ignores case (make lab, 2026-11-21).
function Invoke-Checked([string]$InvokeCheckedWhat, [scriptblock]$InvokeCheckedBlock) {
    $global:LASTEXITCODE = 0
    & $InvokeCheckedBlock
    if ($LASTEXITCODE -ne 0) { throw "$InvokeCheckedWhat failed (exit $LASTEXITCODE)" }
}

function Test-SamePath([string]$First, [string]$Second) {
    if (-not $First -or -not $Second) { return $false }
    [string]::Equals([IO.Path]::GetFullPath($First), [IO.Path]::GetFullPath($Second), [StringComparison]::OrdinalIgnoreCase)
}

function Get-Phase {
    if (Test-Path -LiteralPath $PhaseFile) { [int]((Get-Content -LiteralPath $PhaseFile -Raw).Trim()) } else { 0 }
}
function Set-Phase([int]$Number) { Set-Content -LiteralPath $PhaseFile -Value $Number -Encoding ascii }

# Assert-Rule: the owner's rule. An adapter is allowed only during activation (the new disk built, not
# yet sealed), and only while
# that disk, with no parent, is the only disk and there is no DVD drive. A breach turns the VM off and
# removes the adapter before it stops the script.
function Assert-Rule([string]$When) {
    $adapters = @(Get-VMNetworkAdapter -VMName $VM)
    if ($adapters.Count -eq 0) { return }
    $drives = @(Get-VMHardDiskDrive -VMName $VM)
    $others = @($drives | Where-Object { -not (Test-SamePath $_.Path $state.os_base) })
    $why = ''
    if ((Get-Phase) -ne 2) { $why = 'it is not the activation (phase 3)' }
    elseif ($others.Count) { $why = "other disks are attached: $(($others | ForEach-Object { if ($_.Path) { $_.Path } else { 'a physical disk' } }) -join ', ')" }
    elseif (@(Get-VMDvdDrive -VMName $VM).Count) { $why = 'a DVD drive is attached' }
    elseif ((Get-VHD -Path $state.os_base).ParentPath) { $why = "$($state.os_base) has a parent" }
    if (-not $why) { return }
    Write-Host "RULE BROKEN ($When): $VM has a network adapter and $why. Turning it off and removing the adapter." -ForegroundColor Red
    Stop-VM -Name $VM -TurnOff -Force
    $adapters | Remove-VMNetworkAdapter
    throw "the rule was broken ($When): $why; the VM is off and its adapter removed"
}

# Remove-Adapters removes every network adapter, turning the VM off if a running one keeps any.
function Remove-Adapters {
    if (-not @(Get-VMNetworkAdapter -VMName $VM).Count) { return }
    Write-Host '  removing the network adapter'
    try { Get-VMNetworkAdapter -VMName $VM | Remove-VMNetworkAdapter } catch { Write-Host "  could not remove it while running ($_): turning the VM off" }
    if (@(Get-VMNetworkAdapter -VMName $VM).Count) {
        Stop-VM -Name $VM -TurnOff -Force
        Get-VMNetworkAdapter -VMName $VM | Remove-VMNetworkAdapter
    }
    if (@(Get-VMNetworkAdapter -VMName $VM).Count) { throw "$VM still has a network adapter: remove it in Hyper-V Manager before anything else" }
}

# Stop-Lab shuts the guest down (so the work disk is closed cleanly), and turns it off if it does not.
function Stop-Lab {
    if ((Get-VM -Name $VM).State -eq 'Off') { return }
    Write-Host "  shutting $VM down"
    try { Stop-VM -Name $VM -Force } catch { Write-Host "  the guest did not shut down ($_): turning it off" }
    if ((Get-VM -Name $VM).State -ne 'Off') { Stop-VM -Name $VM -TurnOff -Force }
    if ((Get-VM -Name $VM).State -ne 'Off') { throw "$VM did not turn off" }
}

# Stop-Broker disables the broker's task and waits for the broker to exit, so no lab-run call can start,
# reset or use the VM during the work. It stays disabled until Start-Broker (also after a failure).
function Stop-Broker {
    if (Get-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Disable-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName | Out-Null
        Stop-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName
    }
    $broker = "$Programs\lab-broker.exe"
    $running = { @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path -ieq $broker }) }
    $deadline = (Get-Date).AddSeconds(30)
    while ((& $running).Count -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
    foreach ($process in & $running) { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue; $process.WaitForExit(10000) | Out-Null }
    $script:BrokerStopped = $true
    Write-Host '  the broker is stopped and its task disabled until this finishes'
}

function Test-PipeListening {
    try { [IO.Directory]::GetFiles('\\.\pipe\') -contains "\\.\pipe\$Pipe" } catch { Start-Sleep -Seconds 3; $true }
}

# Test-Lab enables the broker again and proves the VM through it: lab-broker check, lab-run status (the
# host check), then one run, for which the gate starts the VM and probes it.
function Test-Lab {
    Write-Host '=== the broker back, and the gate against the VM'
    if (-not (Get-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -ErrorAction SilentlyContinue)) { throw "the task $TaskPath$TaskName is missing: run install.ps1" }
    Enable-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName | Out-Null
    Start-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName
    $deadline = (Get-Date).AddSeconds(30)
    while (-not (Test-PipeListening) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
    Invoke-Checked 'lab-broker check' { & "$Programs\lab-broker.exe" check }
    Invoke-Checked 'lab-run status' { & "$Programs\lab-run.exe" --project $state.project --vm $VM status }
    # '--' from an array: a bare -- on the line could be taken by PowerShell itself.
    $labRunArguments = @('--project', $state.project, '--vm', $VM, '--', 'hostname')
    Write-Host '--- a run through the gate (it starts the VM and probes it; the first boot takes a few minutes)'
    Invoke-Checked 'lab-run (the gate and its probe)' { & "$Programs\lab-run.exe" @labRunArguments }
}

function Get-GuestPassword {
    Add-Type -AssemblyName System.Security
    $bytes = [Security.Cryptography.ProtectedData]::Unprotect([IO.File]::ReadAllBytes($state.credential), $null, 'LocalMachine')
    try { [Text.Encoding]::UTF8.GetString($bytes) } finally { [Array]::Clear($bytes, 0, $bytes.Length) }
}

function Get-GuestCredential {
    $secure = New-Object Security.SecureString
    foreach ($character in (Get-GuestPassword).ToCharArray()) { $secure.AppendChar($character) }
    New-Object Management.Automation.PSCredential($state.guest_user, $secure)
}

function Wait-Guest($GuestCredential, [int]$Minutes) {
    $deadline = (Get-Date).AddMinutes($Minutes)
    while ($true) {
        try { return Invoke-Command -VMName $VM -Credential $GuestCredential -ScriptBlock { $env:COMPUTERNAME } -ErrorAction Stop } catch {
            if ((Get-Date) -gt $deadline) { throw "PowerShell Direct did not answer within $Minutes minutes: $_" }
            Start-Sleep -Seconds 15
        }
    }
}

# Get-GuestLicense reads Windows' license in the guest: LicenseStatus 1 is Licensed; GraceMinutes is
# what slmgr /dlv calls the time-based activation's expiration.
function Get-GuestLicense($GuestCredential) {
    Invoke-Command -VMName $VM -Credential $GuestCredential -ArgumentList $WindowsApplication -ScriptBlock {
        param($application)
        $product = Get-CimInstance -ClassName SoftwareLicensingProduct -Filter "ApplicationID='$application' AND PartialProductKey IS NOT NULL" | Select-Object -First 1
        $evaluationEnd = ''
        try { if ($product.EvaluationEndDate) { $evaluationEnd = $product.EvaluationEndDate.ToString('yyyy-MM-dd') } } catch { }
        [pscustomobject]@{
            Name = "$($product.Name)"; Description = "$($product.Description)"
            Status = [int]$product.LicenseStatus; GraceMinutes = [long]$product.GracePeriodRemaining; EvaluationEnd = $evaluationEnd
        }
    }
}

# Get-GuestDefrag reports what of the guest's drive optimisation is still on ("" when nothing is): a
# Defrag task not disabled, defragsvc not disabled or running, a defrag.exe running.
function Get-GuestDefrag($GuestCredential) {
    Invoke-Command -VMName $VM -Credential $GuestCredential -ScriptBlock {
        $on = @(Get-ScheduledTask -TaskPath '\Microsoft\Windows\Defrag\' -ErrorAction SilentlyContinue | Where-Object { $_.State -ne 'Disabled' } | ForEach-Object { "task $($_.TaskName) $($_.State)" })
        $service = Get-Service -Name defragsvc -ErrorAction SilentlyContinue
        if ($service -and ("$($service.StartType)" -ne 'Disabled' -or "$($service.Status)" -ne 'Stopped')) { $on += "defragsvc $($service.StartType) $($service.Status)" }
        if (@(Get-Process -Name defrag -ErrorAction SilentlyContinue).Count) { $on += 'defrag.exe running' }
        $on -join ', '
    }
}

# Disable-GuestDefrag stops and disables the guest's drive optimisation, and checks it.
function Disable-GuestDefrag($GuestCredential) {
    Invoke-Command -VMName $VM -Credential $GuestCredential -ScriptBlock {
        Get-ScheduledTask -TaskPath '\Microsoft\Windows\Defrag\' -ErrorAction SilentlyContinue | Stop-ScheduledTask -ErrorAction SilentlyContinue
        Get-Process -Name defrag -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
        Get-ScheduledTask -TaskPath '\Microsoft\Windows\Defrag\' -ErrorAction SilentlyContinue | Disable-ScheduledTask | Out-Null
        Set-Service -Name defragsvc -StartupType Disabled
        Stop-Service -Name defragsvc -Force -ErrorAction SilentlyContinue
    }
    $left = Get-GuestDefrag $GuestCredential
    if ($left) { throw "drive optimisation is still on in the guest: $left" }
}

# Assert-LabDisks: the VM's disks are exactly its lab state's, os_diff over os_base (the gate's check).
function Assert-LabDisks {
    $drives = @(Get-VMHardDiskDrive -VMName $VM)
    $recorded = @($state.disks)
    if ($drives.Count -ne $recorded.Count) { throw "$VM has $($drives.Count) disks; its lab state records $($recorded.Count) ($($recorded -join ', ')). Fix that first" }
    foreach ($drive in $drives) {
        if (-not $drive.Path) { throw "$VM has a physical disk passed through" }
        if (-not @($recorded | Where-Object { Test-SamePath $_ $drive.Path }).Count) { throw "$VM has a disk its lab state does not list: $($drive.Path)" }
    }
    if (-not (Test-SamePath (Get-VHD -Path $state.os_diff).ParentPath $state.os_base)) { throw "$($state.os_diff) is not over $($state.os_base)" }
}

function Read-Attachments {
    if (-not (Test-Path -LiteralPath $AttachmentsFile)) { throw "$AttachmentsFile is missing: the disks were never recorded" }
    # Assigned first, then enumerated: Windows PowerShell 5.1 emits a JSON array as one object.
    $records = Get-Content -LiteralPath $AttachmentsFile -Raw | ConvertFrom-Json
    foreach ($record in $records) { $record }
}

# Grant-VMAccess lets the VM's own account open Path. Hyper-V grants it when it attaches a disk, but
# not to a file made later under a path already attached (Seal's new os_diff): Start-VM then fails
# with "Account does not have permission to open attachment" (lab-run reset, 2026-11-23).
function Grant-VMAccess([string]$Path) {
    $account = 'NT VIRTUAL MACHINE\' + (Get-VM -Name $VM).Id
    Invoke-Checked "granting $account on $Path" { icacls $Path /grant "$($account):(F)" /Q | Out-Null }
}

# Restore-Attachments puts every recorded disk back at its controller and location, never while an
# adapter is attached, and boots from os_diff.
function Restore-Attachments {
    if (@(Get-VMNetworkAdapter -VMName $VM).Count) { throw "$VM has a network adapter: not attaching its disks" }
    foreach ($record in Read-Attachments) {
        Add-VMHardDiskDrive -VMName $VM -ControllerType $record.controller_type -ControllerNumber $record.controller_number -ControllerLocation $record.controller_location -Path $record.path
        # Also repairs a child a reset made without the grant.
        Grant-VMAccess $record.path
    }
    Grant-VMAccess $state.os_base
    $boot = Get-VMHardDiskDrive -VMName $VM | Where-Object { Test-SamePath $_.Path $state.os_diff } | Select-Object -First 1
    Set-VMFirmware -VMName $VM -FirstBootDevice $boot
}

function Remove-Disk([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return }
    try { Dismount-VHD -Path $Path -ErrorAction Stop } catch { }
    Set-ItemProperty -LiteralPath $Path -Name IsReadOnly -Value $false
    Remove-Item -LiteralPath $Path -Force
}

function New-OSDiff {
    # Removed only when the old one is set aside: then whatever is at the path is this rebase's.
    if ($RebaseInProgress -and -not (Test-Path -LiteralPath $OldDiff)) { throw "the old os_diff is not in ${OldDiff}: not replacing $($state.os_diff)" }
    Remove-Disk $state.os_diff
    New-VHD -Path $state.os_diff -ParentPath $state.os_base -Differencing | Out-Null
}

function Show-Expiry {
    $file = Join-Path $RebaseDir 'activation.txt'
    if (Test-Path -LiteralPath $file) {
        Write-Host ''
        Get-Content -LiteralPath $file | ForEach-Object { Write-Host "  $_" -ForegroundColor Green }
    }
}

# --- the phases

function Invoke-Prepare {
    Write-Host '=== 1. prepare: the VM off, its disks recorded and detached, the old system disks set aside'
    Assert-Rule 'prepare, start'
    if (-not (Test-Path -LiteralPath $AttachmentsFile)) {
        Stop-Lab
        Assert-LabDisks
        New-Item -ItemType Directory -Force -Path $RebaseDir | Out-Null
        $records = @(Get-VMHardDiskDrive -VMName $VM | ForEach-Object {
                [ordered]@{ path = $_.Path; controller_type = "$($_.ControllerType)"; controller_number = $_.ControllerNumber; controller_location = $_.ControllerLocation }
            })
        ConvertTo-Json -InputObject $records -Depth 3 | Set-Content -LiteralPath $AttachmentsFile -Encoding utf8
        foreach ($record in $records) { Write-Host "  recorded $($record.path) at $($record.controller_type) $($record.controller_number):$($record.controller_location)" }
    }
    Stop-Lab
    Get-VMHardDiskDrive -VMName $VM | Remove-VMHardDiskDrive
    Get-VMDvdDrive -VMName $VM | Remove-VMDvdDrive
    foreach ($pair in @(@($state.os_diff, $OldDiff), @($state.os_base, $OldBase))) {
        $here = Test-Path -LiteralPath $pair[0]
        $there = Test-Path -LiteralPath $pair[1]
        if ($here -and $there) { throw "both $($pair[0]) and $($pair[1]) exist: sort that out by hand, or -Rollback" }
        if ($here) { Move-Item -LiteralPath $pair[0] -Destination $pair[1]; Write-Host "  set aside: $($pair[1])" }
        elseif (-not $there) { throw "$($pair[0]) is missing, and not set aside at $($pair[1])" }
    }
    Assert-Rule 'prepare, end'
    Set-Phase 1
}

function Resolve-Iso {
    $media = Join-Path $Data 'media'
    New-Item -ItemType Directory -Force -Path $media | Out-Null
    $wanted = $IsoSha256.ToLowerInvariant()
    if ($Iso) {
        if ($wanted -notmatch '^[0-9a-f]{64}$') { throw '-Iso needs -IsoSha256 (64 hex digits)' }
    } elseif (-not $wanted) {
        $pinned = @(Get-ChildItem -LiteralPath $media -Filter *.iso | Where-Object { $_.BaseName -match '^[0-9a-fA-F]{64}$' })
        if ($pinned.Count -ne 1) { throw "$media holds $($pinned.Count) pinned ISOs (<sha256>.iso); pass -Iso PATH -IsoSha256 HEX" }
        $wanted = $pinned[0].BaseName.ToLowerInvariant()
    }
    $copy = Join-Path $media "$wanted.iso"
    # Copied into the admin-only media folder first, so the file hashed is the file mounted.
    if (-not (Test-Path -LiteralPath $copy)) {
        if (-not $Iso) { throw "$copy is missing; pass -Iso PATH" }
        Write-Host "  copying $Iso into $media"
        Copy-Item -LiteralPath $Iso -Destination $copy
    }
    Write-Host "  hashing $copy"
    $hash = (Get-FileHash -LiteralPath $copy -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($hash -ne $wanted) {
        Remove-Item -LiteralPath $copy -Force
        throw "the ISO's SHA-256 is $hash, not ${wanted}: not using it (the copy is removed)"
    }
    Write-Host "  SHA-256 $hash matches"
    $copy
}

function Invoke-Build {
    Write-Host '=== 2. build: Windows applied offline onto a new system disk'
    Assert-Rule 'build, start'
    if (@(Get-VMHardDiskDrive -VMName $VM).Count) { throw "$VM has disks attached; after prepare it has none" }
    if (-not (Test-Path -LiteralPath $OldBase)) { throw "the old os_base is not in ${OldBase}: not building over $($state.os_base)" }
    if (Test-Path -LiteralPath $state.os_base) { Write-Host "  removing an unfinished $($state.os_base) from an earlier attempt"; Remove-Disk $state.os_base }
    # Room for the applied image (about 25 GB) and the gate's 10 GiB floor, with margin.
    $free = (New-Object IO.DriveInfo([IO.Path]::GetPathRoot($state.os_base))).AvailableFreeSpace
    if ($free -lt 50GB) { throw "the volume holding $($state.os_base) has $([math]::Round($free / 1GB)) GB free; the build needs 50 GB" }
    $isoCopy = Resolve-Iso
    $sizeBytes = (Get-VHD -Path $OldBase).Size
    $gpu = [bool]@(Get-VMGpuPartitionAdapter -VMName $VM -ErrorAction SilentlyContinue).Count

    $isoImage = Mount-DiskImage -ImagePath $isoCopy -PassThru
    try {
        $isoDrive = ($isoImage | Get-Volume).DriveLetter + ':'
        $signature = Get-AuthenticodeSignature "$isoDrive\setup.exe"
        if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch '^CN=Microsoft Corporation,') {
            throw "the ISO is not a Microsoft image: setup.exe signature '$($signature.Status)', signer '$($signature.SignerCertificate.Subject)'"
        }
        $wim = @("$isoDrive\sources\install.wim", "$isoDrive\sources\install.esd") | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
        if (-not $wim) { throw 'no install.wim or install.esd in the ISO' }
        $images = @(Get-WindowsImage -ImagePath $wim)
        $image = $images | Where-Object { $_.ImageName -match 'Enterprise' -and $_.ImageName -match 'Evaluation' -and $_.ImageName -notmatch ' N( |$)' } | Select-Object -First 1
        if (-not $image) { throw "no Enterprise Evaluation edition in the ISO: $(($images | ForEach-Object { $_.ImageName }) -join '; ')" }
        Write-Host "  applying '$($image.ImageName)' (index $($image.ImageIndex)) to $($state.os_base), $([math]::Round($sizeBytes / 1GB)) GB"

        New-VHD -Path $state.os_base -SizeBytes $sizeBytes -Dynamic | Out-Null
        $disk = Mount-VHD -Path $state.os_base -Passthru | Get-Disk
        try {
            Initialize-Disk -Number $disk.Number -PartitionStyle GPT
            $efi = New-Partition -DiskNumber $disk.Number -Size 260MB -GptType '{c12a7328-f81f-11d2-ba4b-00a0c93ec93b}' -AssignDriveLetter
            Format-Volume -Partition $efi -FileSystem FAT32 -NewFileSystemLabel 'System' -Confirm:$false | Out-Null
            New-Partition -DiskNumber $disk.Number -Size 16MB -GptType '{e3c9e316-0b5c-4db8-817d-f92df00215ae}' | Out-Null
            $windows = New-Partition -DiskNumber $disk.Number -UseMaximumSize -AssignDriveLetter
            Format-Volume -Partition $windows -FileSystem NTFS -NewFileSystemLabel 'Windows' -Confirm:$false | Out-Null
            $target = "$($windows.DriveLetter):"
            $efiTarget = "$($efi.DriveLetter):"
            Expand-WindowsImage -ImagePath $wim -Index $image.ImageIndex -ApplyPath "$target\" | Out-Null
            Invoke-Checked 'bcdboot' { bcdboot "$target\Windows" /s $efiTarget /f UEFI | Out-Null }

            # The answer file: the lab state's computer name, guest_user with the password already in
            # the credential blob, no online OOBE. Setup masks the password in its own copy; activate
            # removes the file once the guest is up.
            $passwordXml = [Security.SecurityElement]::Escape((Get-GuestPassword))
            $unattend = @"
<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>$($state.hostname)</ComputerName>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>en-US</InputLocale><SystemLocale>en-US</SystemLocale><UILanguage>en-US</UILanguage><UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE>
        <HideEULAPage>true</HideEULAPage><HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens><HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <HideLocalAccountScreen>true</HideLocalAccountScreen><ProtectYourPC>3</ProtectYourPC>
      </OOBE>
      <UserAccounts><LocalAccounts>
        <LocalAccount wcm:action="add">
          <Name>$($state.guest_user)</Name><Group>Administrators</Group>
          <Password><Value>$passwordXml</Value><PlainText>true</PlainText></Password>
        </LocalAccount>
      </LocalAccounts></UserAccounts>
      <TimeZone>UTC</TimeZone>
    </component>
  </settings>
</unattend>
"@
            $passwordXml = $null
            New-Item -ItemType Directory -Force "$target\Windows\Panther" | Out-Null
            [IO.File]::WriteAllText("$target\Windows\Panther\unattend.xml", $unattend, (New-Object Text.UTF8Encoding($false)))
            $unattend = $null

            # GPU partitioning: the host's NVIDIA driver files, where a partitioned guest looks for them.
            if ($gpu) {
                $nvidia = Get-CimInstance Win32_VideoController | Where-Object Name -match 'NVIDIA' | Select-Object -First 1
                if (-not $nvidia) { throw "$VM has a GPU partition but the host has no NVIDIA adapter" }
                $folders = @(($nvidia.InstalledDisplayDrivers -split ',') | ForEach-Object {
                        if ($_ -match '\\FileRepository\\([^\\]+)\\') { $Matches[1] } } | Select-Object -Unique)
                $hostStore = "$target\Windows\System32\HostDriverStore\FileRepository"
                New-Item -ItemType Directory -Force $hostStore | Out-Null
                foreach ($folder in $folders) {
                    Write-Host "  GPU driver folder $folder"
                    Copy-Item -Recurse -Force "C:\Windows\System32\DriverStore\FileRepository\$folder" $hostStore
                }
                Get-ChildItem C:\Windows\System32 -Filter 'nv*.dll' | ForEach-Object {
                    if (-not (Test-Path -LiteralPath "$target\Windows\System32\$($_.Name)")) { Copy-Item -LiteralPath $_.FullName -Destination "$target\Windows\System32\" }
                }
            }
        } finally { Dismount-VHD -Path $state.os_base }
    } finally { Dismount-DiskImage -ImagePath $isoCopy | Out-Null }
    Assert-Rule 'build, end'
    Set-Phase 2
}

function Invoke-Activate {
    Write-Host '=== 3. activate: the new system disk alone in the VM, online once'
    Remove-Adapters
    Stop-Lab
    Assert-Rule 'activate, start'
    Get-VMHardDiskDrive -VMName $VM | Remove-VMHardDiskDrive
    $slot = Read-Attachments | Where-Object { Test-SamePath $_.path $state.os_diff } | Select-Object -First 1
    Add-VMHardDiskDrive -VMName $VM -ControllerType $slot.controller_type -ControllerNumber $slot.controller_number -ControllerLocation $slot.controller_location -Path $state.os_base
    Set-VMFirmware -VMName $VM -FirstBootDevice (Get-VMHardDiskDrive -VMName $VM | Select-Object -First 1)
    Write-Host "  starting $VM with only $($state.os_base) (first boot: specialize and OOBE, several minutes)"
    Start-VM -Name $VM
    $guestCredential = Get-GuestCredential
    $name = Wait-Guest $guestCredential 30
    if ($name -ne $state.hostname) { throw "the guest is '$name'; the lab state says '$($state.hostname)'" }
    Write-Host "  PowerShell Direct answers as $($state.guest_user) on $name"
    # Windows' default local password age is 42 days, less than an evaluation's 90. Drive
    # optimisation off, and no last-access writes: a guest rewrote its game volume in one boot, growing
    # the game disk's child by tens of GB until the host's C: was full (2026-11-23). The Defrag task alone
    # did not stay off (something re-enabled it about 10 minutes after the first boot), so the service
    # defrag.exe needs (defragsvc, Optimize Drives) is disabled too, and both are checked after a reboot.
    Invoke-Command -VMName $VM -Credential $guestCredential -ArgumentList $state.guest_user -ScriptBlock {
        param($user)
        Set-LocalUser -Name $user -PasswordNeverExpires $true
        Remove-Item -LiteralPath 'C:\Windows\Panther\unattend.xml' -Force -ErrorAction SilentlyContinue
        & fsutil.exe behavior set disablelastaccess 1 | Out-Null
    }
    Disable-GuestDefrag $guestCredential

    $license = Get-GuestLicense $guestCredential
    if ($license.Status -ne 1) {
        Assert-Rule 'before the adapter'
        $drives = @(Get-VMHardDiskDrive -VMName $VM)
        if ($drives.Count -ne 1 -or -not (Test-SamePath $drives[0].Path $state.os_base)) { throw "$VM has other disks than $($state.os_base): no adapter" }
        if ((Get-VHD -Path $state.os_base).ParentPath) { throw "$($state.os_base) has a parent: no adapter" }
        if (@(Get-VMDvdDrive -VMName $VM).Count) { throw "$VM has a DVD drive: no adapter" }
        $switch = Get-VMSwitch | Where-Object { "$($_.Id)" -eq $DefaultSwitchId -or ($_.Name -eq 'Default Switch' -and "$($_.SwitchType)" -eq 'Internal') } | Select-Object -First 1
        if (-not $switch) { throw "Hyper-V's Default Switch is missing" }
        Write-Host "  adding a network adapter on '$($switch.Name)' to activate Windows"
        try {
            Add-VMNetworkAdapter -VMName $VM -SwitchName $switch.Name -Name 'rebase-activation'
            Assert-Rule 'after adding the adapter'
            $deadline = (Get-Date).AddMinutes(15)
            while ($true) {
                Assert-Rule 'during activation'
                try {
                    $said = Invoke-Command -VMName $VM -Credential $guestCredential -ScriptBlock {
                        $ErrorActionPreference = 'Continue'
                        (& cscript.exe //nologo "$env:SystemRoot\System32\slmgr.vbs" /ato 2>&1 | ForEach-Object { "$_" }) -join ' '
                    }
                    Write-Host "  slmgr /ato: $("$said".Trim())"
                    $license = Get-GuestLicense $guestCredential
                } catch { Write-Host "  not yet: $_" }
                if ($license.Status -eq 1) { break }
                if ((Get-Date) -gt $deadline) { throw "Windows did not activate within 15 minutes (license status $($license.Status): $($license.Description)). Run the same line again to retry" }
                Start-Sleep -Seconds 30
            }
        } finally { Remove-Adapters }
    }
    Assert-Rule 'activate, end'
    if (@(Get-VMNetworkAdapter -VMName $VM).Count) { throw "$VM still has a network adapter" }

    # Checked again with no adapter.
    $license = Get-GuestLicense $guestCredential
    if ($license.Status -ne 1) { throw "the license reads status $($license.Status) after activation: $($license.Description)" }
    $ends = (Get-Date).AddMinutes($license.GraceMinutes)
    $lines = @(
        "Activated $(Get-Date -Format 'yyyy-MM-dd HH:mm'): $($license.Name)",
        "  $($license.Description)",
        "  the activation ends about $($ends.ToString('yyyy-MM-dd')) ($([math]::Floor($license.GraceMinutes / 1440)) days): rebase $VM before then",
        "  evaluation end date: $(if ($license.EvaluationEnd) { $license.EvaluationEnd } else { 'not reported' })"
    )
    Set-Content -LiteralPath (Join-Path $RebaseDir 'activation.txt') -Value $lines -Encoding ascii
    foreach ($line in $lines) { Write-Host "  $line" }
    # Once more after a full boot: whatever re-enabled the task must not have re-enabled either.
    Write-Host "  restarting $VM to check drive optimisation stays off"
    Stop-Lab
    Start-VM -Name $VM
    Wait-Guest $guestCredential 15 | Out-Null
    $left = Get-GuestDefrag $guestCredential
    if ($left) { throw "after a reboot drive optimisation is on again ($left); run the same line again to retry" }
    Write-Host '  after a reboot: the Defrag tasks and defragsvc are disabled'
    $guestCredential = $null
    Stop-Lab
    Set-Phase 3
}

function Invoke-Finish {
    Write-Host '=== 4. finish: the new disk sealed as os_base, the lab disks back where they were'
    Stop-Lab
    Remove-Adapters
    Assert-Rule 'finish, start'
    Get-VMHardDiskDrive -VMName $VM | Remove-VMHardDiskDrive
    Set-ItemProperty -LiteralPath $state.os_base -Name IsReadOnly -Value $true
    New-OSDiff
    Restore-Attachments
    Assert-LabDisks
    Assert-Rule 'finish, end'
    Set-Phase 4
}

# --- the other modes

function Invoke-Rollback {
    Write-Host '=== rollback: the old system disks back'
    if (-not (Test-Path -LiteralPath $RebaseDir)) { throw "no rebase of $VM to roll back ($RebaseDir is missing)" }
    if (-not (Test-Path -LiteralPath $AttachmentsFile)) {
        # Prepare stopped before recording the disks, so it changed nothing.
        Remove-Item -LiteralPath $RebaseDir -Recurse -Force
        Test-Lab
        Write-Host "Rolled back: the rebase had changed nothing." -ForegroundColor Green
        return
    }
    Stop-Broker
    Stop-Lab
    Remove-Adapters
    Get-VMDvdDrive -VMName $VM | Remove-VMDvdDrive
    Get-VMHardDiskDrive -VMName $VM | Remove-VMHardDiskDrive
    # A path's file is this rebase's only while the old one is set aside.
    foreach ($pair in @(@($state.os_diff, $OldDiff), @($state.os_base, $OldBase))) {
        if (Test-Path -LiteralPath $pair[1]) {
            Remove-Disk $pair[0]
            Move-Item -LiteralPath $pair[1] -Destination $pair[0]
            Write-Host "  restored $($pair[0])"
        }
    }
    Restore-Attachments
    Assert-LabDisks
    Assert-Rule 'rollback'
    $kept = "$RebaseDir-rolled-back-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    Move-Item -LiteralPath $RebaseDir -Destination $kept
    Write-Host "  this rebase's records: $kept"
    Test-Lab
    Write-Host "Rolled back: $VM has its old system disk." -ForegroundColor Green
}

function Invoke-Seal {
    Write-Host '=== seal: os_diff folded into os_base'
    if ((Test-Path -LiteralPath $RebaseDir) -and (Get-Phase) -lt 4) { throw "a rebase of $VM is not finished: run it first (the same line without -Seal)" }
    Stop-Broker
    Stop-Lab
    if (@(Get-VMNetworkAdapter -VMName $VM).Count) { throw "$VM has a network adapter: not sealing" }
    Assert-LabDisks
    Write-Host "  merging $($state.os_diff) into $($state.os_base)"
    Set-ItemProperty -LiteralPath $state.os_base -Name IsReadOnly -Value $false
    Merge-VHD -Path $state.os_diff -DestinationPath $state.os_base
    Set-ItemProperty -LiteralPath $state.os_base -Name IsReadOnly -Value $true
    New-VHD -Path $state.os_diff -ParentPath $state.os_base -Differencing | Out-Null
    Grant-VMAccess $state.os_diff
    Assert-LabDisks
    Test-Lab
    Write-Host "Sealed: lab-run reset now returns $VM to this point." -ForegroundColor Green
}

function Invoke-Forget {
    Write-Host '=== forget: the old disks of a finished rebase deleted'
    if (-not (Test-Path -LiteralPath $RebaseDir)) { throw "no rebase of $VM is kept ($RebaseDir is missing)" }
    if ((Get-Phase) -lt 4) { throw "the rebase of $VM is not finished: finish it (the same line without -Forget) or -Rollback" }
    $old = @(Get-ChildItem -LiteralPath $RebaseDir -Filter *.vhdx)
    foreach ($file in $old) { Write-Host "  $($file.FullName) ($([math]::Round($file.Length / 1GB, 1)) GB)" }
    if ((Read-Host 'Type DELETE to delete these for good (no rollback afterwards)') -cne 'DELETE') { throw 'not confirmed; nothing deleted' }
    foreach ($file in $old) { Remove-Disk $file.FullName }
    $kept = Join-Path (Split-Path -Parent $RebaseDir) "rebase-done-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    Move-Item -LiteralPath $RebaseDir -Destination $kept
    Write-Host "Deleted. This rebase's records: $kept" -ForegroundColor Green
}

try {
    $StateFile = Join-Path $Data "labs\$VM.json"
    if ($VM -notmatch '^[A-Za-z0-9][A-Za-z0-9-]{0,63}$') { throw "VM name ${VM}: letters, digits and '-' only" }
    $state = Get-Content -LiteralPath $StateFile -Raw | ConvertFrom-Json
    if ($state.vm -ne $VM) { throw "$StateFile names VM $($state.vm)" }
    if (-not (Get-VM -Name $VM -ErrorAction SilentlyContinue)) { throw "Hyper-V has no VM $VM" }
    if ((Split-Path -Leaf $state.os_base) -ieq (Split-Path -Leaf $state.os_diff)) { throw 'os_base and os_diff have the same file name' }
    $RebaseDir = Join-Path (Split-Path -Parent $state.os_base) 'rebase'
    $PhaseFile = Join-Path $RebaseDir 'phase.txt'
    $AttachmentsFile = Join-Path $RebaseDir 'attachments.json'
    $OldBase = Join-Path $RebaseDir ('old-' + (Split-Path -Leaf $state.os_base))
    $OldDiff = Join-Path $RebaseDir ('old-' + (Split-Path -Leaf $state.os_diff))
    $RebaseInProgress = (Test-Path -LiteralPath $RebaseDir) -and (Get-Phase) -lt 4
    Write-Host "Lab $VM (project $($state.project)): $((Get-VM -Name $VM).State)"
    Assert-Rule 'at the start'

    if ($Rollback) { Invoke-Rollback }
    elseif ($Seal) { Invoke-Seal }
    elseif ($Forget) { Invoke-Forget }
    else {
        if ((Get-Phase) -ge 4) {
            Write-Host "The rebase of $VM is finished; checking it again. Its old disks are kept in $RebaseDir (-Rollback puts them back; -Forget deletes them, and a new rebase needs that first)."
        } else {
            if (Test-Path -LiteralPath $PhaseFile) { Write-Host "Resuming after phase $(Get-Phase)" }
            Stop-Broker
            if ((Get-Phase) -lt 1) { Invoke-Prepare }
            if ((Get-Phase) -lt 2) { Invoke-Build }
            if ((Get-Phase) -lt 3) { Invoke-Activate }
            if ((Get-Phase) -lt 4) { Invoke-Finish }
        }
        Test-Lab
        Show-Expiry
        Write-Host ''
        Write-Host "Rebased: $VM has a fresh, activated system disk; its game and work disks are as they were." -ForegroundColor Green
        Write-Host 'In the guest everything installed on the old system disk is gone (tools, mount points): set it up again, then -Seal.'
    }
    exit 0
} catch {
    Write-Host "FAILED: $_" -ForegroundColor Red
    try {
        if ($state -and (Get-VM -Name $VM -ErrorAction SilentlyContinue) -and @(Get-VMNetworkAdapter -VMName $VM).Count -and -not $Rollback -and -not $Seal) {
            # Only activation adds an adapter, and its own finally removes it; this is the last net.
            Remove-Adapters
        }
    } catch { Write-Host "AND the network adapter could not be removed: $_ (remove it in Hyper-V Manager now)" -ForegroundColor Red }
    if ($PhaseFile -and (Test-Path -LiteralPath $RebaseDir) -and (Get-Phase) -lt 4 -and -not $Forget) {
        Write-Host 'The broker stays disabled while the VM is not in its lab state. Run the same line again to continue, or add -Rollback to put the old system disk back.'
    } elseif ($script:BrokerStopped) {
        # No rebase is half done: the gate decides about the VM, as always.
        try {
            Enable-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName | Out-Null
            Start-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName
            Write-Host 'The broker is running again; its gate refuses the VM until it matches its lab state.'
        } catch { Write-Host "The broker's task could not be enabled again ($_): run install.ps1" -ForegroundColor Red }
    }
    exit 1
}
