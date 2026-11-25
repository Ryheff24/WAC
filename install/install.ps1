#Requires -RunAsAdministrator
# install.ps1: the Windows side of the agent containment, which is only the lab: the SYSTEM broker
# that proves a lab VM has no network before every call, and lab-run.exe, its client. The agents
# themselves run in WSL, in Claude Code's sandbox (wsl/install.sh); /usr/local/bin/lab-run there runs
# this lab-run.exe.
#
# Run it from an elevated PowerShell (Windows PowerShell 5.1 or PowerShell 7):
#   powershell -NoProfile -ExecutionPolicy Bypass -File install\install.ps1
#
#   1. clones main of the repository fresh into an admin-only directory
#      (C:\ProgramData\agent-containment\src\<time>), with Git and Go from C:\Program Files and none
#      of your profile's configuration: what runs as SYSTEM is built only from there
#   2. go vet, the suite, and builds lab-broker.exe and lab-run.exe
#   3. installs both into C:\Program Files\agent-containment\ (only administrators write there), with
#      rebase-os.ps1, which the owner runs from there to give a lab a fresh system disk
#   4. the registry (registry\projects\*.json) into C:\ProgramData\agent-containment\projects\, and
#      each project's lab exchange (its root, with lab-in\: WSL's sandbox may write only lab-in)
#   5. records your account as every lab state's client (lab-broker set-owner): the broker's pipe
#      admits it, SYSTEM and Administrators, nobody else. A state it changes is copied first to
#      C:\ProgramData\agent-containment\labs.before-<time>\
#   6. the host's Enhanced Session mode off (the gate refuses a lab while it is on)
#   7. one scheduled task, \agent-containment\lab-broker: SYSTEM, at startup, restarts on failure
#   8. lab-broker check, then lab-run status for every lab: the gate against the real VM
# Any failure stops it with a FAILED line and exit 1. Running it again is an update. It never builds,
# changes or starts a VM other than through the gate. uninstall.ps1 removes all of it but the labs.
#
# -Branch builds another branch of the repository instead of main (to try a pull request before it
# is merged). The script must be that branch's own install.ps1, or it stops before installing. -Owner names the account lab-run runs as, if it is not the one running this.

param([string]$Branch = 'main', [string]$Owner = '')

$ErrorActionPreference = 'Stop'
$Repository = 'https://github.com/Ryheff24/WAC.git'
$Programs = 'C:\Program Files\agent-containment'
$Data = 'C:\ProgramData\agent-containment'
$Git = 'C:\Program Files\Git\cmd\git.exe'
$Go = 'C:\Program Files\Go\bin\go.exe'
$TaskPath = '\agent-containment\'
$TaskName = 'lab-broker'
$Pipe = 'agent-containment-lab'
# SYSTEM, Administrators, TrustedInstaller
$Trusted = @('S-1-5-18', 'S-1-5-32-544', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
$AdminOnly = @('*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F')
$AdminAndUsersRead = @('*S-1-5-18:(OI)(CI)F', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-32-545:(OI)(CI)RX')

function Get-OwnerSid([string]$Path) {
    (Get-Acl -LiteralPath $Path).GetOwner([Security.Principal.SecurityIdentifier]).Value
}

# Invoke-Checked runs a native program and throws on a non-zero exit. Its parameters have names no
# caller uses: PowerShell scopes variables dynamically and ignores case (make lab, 2026-11-21).
function Invoke-Checked([string]$InvokeCheckedWhat, [scriptblock]$InvokeCheckedBlock) {
    $global:LASTEXITCODE = 0
    & $InvokeCheckedBlock
    if ($LASTEXITCODE -ne 0) { throw "$InvokeCheckedWhat failed (exit $LASTEXITCODE)" }
}

# Initialize-OwnedDirectory creates Path, or takes it over only if it (and, unless -Shallow,
# everything in it) already belongs to SYSTEM, Administrators or TrustedInstaller and is no link: a
# directory someone else made first is refused, not adopted. Then Administrators own it and Grants
# replace its access list; unless -Shallow, everything in it is reset to inherit them.
function Initialize-OwnedDirectory([string]$Path, [string[]]$Grants, [switch]$Shallow) {
    if (Test-Path -LiteralPath $Path) {
        $entries = @(Get-Item -LiteralPath $Path -Force)
        if (-not $Shallow) { $entries += @(Get-ChildItem -LiteralPath $Path -Recurse -Force) }
        foreach ($entry in $entries) {
            if ($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "$($entry.FullName) is a link or junction; remove it and run this again" }
            $owner = Get-OwnerSid $entry.FullName
            if ($Trusted -notcontains $owner) {
                throw "$($entry.FullName) exists and is owned by $owner, not by SYSTEM or Administrators: it may have been planted. Remove $Path yourself, then run this again"
            }
        }
    } else {
        New-Item -ItemType Directory -Force -Path $Path | Out-Null
    }
    # @( ) keeps it an array: a one-element array unwrapped to a string reaches icacls one character
    # at a time (make lab, 2026-11-21).
    $recurse = @(if (-not $Shallow) { '/T' })
    icacls $Path /setowner '*S-1-5-32-544' @recurse /C /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls /setowner $Path failed ($LASTEXITCODE)" }
    # The grants go on the directory only; then everything in it is reset to inherit them (with /T,
    # icacls left each file with an empty access list: make lab, 2026-11-21).
    icacls $Path /inheritance:r /grant:r @Grants /C /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls $Path failed ($LASTEXITCODE)" }
    if (-not $Shallow -and (Get-ChildItem -LiteralPath $Path -Force | Select-Object -First 1)) {
        icacls (Join-Path $Path '*') /reset /T /C /Q | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "icacls /reset under $Path failed ($LASTEXITCODE)" }
    }
    if ((Get-OwnerSid $Path) -ne 'S-1-5-32-544') { throw "$Path is not owned by Administrators after setting it" }
}

# Stop-InstalledProcess waits up to Seconds for every process running Path to exit, then stops the
# ones left (Stop-ScheduledTask returns before the broker has exited: make lab, 2026-11-21).
function Stop-InstalledProcess([string]$Path, [int]$Seconds = 30) {
    $running = { @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path -ieq $Path }) }
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((& $running).Count -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
    foreach ($process in & $running) {
        Write-Host "  stopping $Path (process $($process.Id))"
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        $process.WaitForExit(10000) | Out-Null
    }
}

# Install-File copies Source over Destination. A program still running (a lab-run call) cannot be
# overwritten but can be renamed: then it is moved aside as <name>.old-<time>, removed by a later run.
function Install-File([string]$Source, [string]$Destination) {
    Get-ChildItem -LiteralPath (Split-Path $Destination) -Filter "$(Split-Path $Destination -Leaf).old-*" -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
    try {
        Copy-Item -LiteralPath $Source -Destination $Destination -Force -ErrorAction Stop
    } catch [System.IO.IOException] {
        $aside = "$Destination.old-$(Get-Date -Format yyyyMMddHHmmss)"
        Write-Host "  $Destination is in use: moving it aside to $aside"
        Move-Item -LiteralPath $Destination -Destination $aside -Force
        Copy-Item -LiteralPath $Source -Destination $Destination -Force -ErrorAction Stop
    }
}

# Test-PipeListening lists the pipes without opening the broker's (opening it would be a call). If
# they cannot be listed (a pipe name .NET refuses), it waits a little and lets lab-run status tell.
function Test-PipeListening {
    try { [IO.Directory]::GetFiles('\\.\pipe\') -contains "\\.\pipe\$Pipe" } catch { Start-Sleep -Seconds 3; $true }
}

try {
    # Nothing from the caller's environment reaches a program this runs (review 2026-11-21, A9): a
    # fixed PATH before the first one (icacls, git, go), no inherited git, Go or compiler variables,
    # and modules only from the system's own folders.
    $env:PATH = "C:\Windows\System32;C:\Windows;C:\Windows\System32\WindowsPowerShell\v1.0;$(Split-Path $Git);$(Split-Path $Go)"
    $env:PSModulePath = "$PSHOME\Modules;$env:SystemRoot\System32\WindowsPowerShell\v1.0\Modules;$env:ProgramFiles\WindowsPowerShell\Modules"
    Get-ChildItem Env: | Where-Object { $_.Name -match '^(GIT_|GO|CGO_|CC$|CXX$|AR$|PKG_CONFIG)' } | ForEach-Object { Remove-Item -LiteralPath "Env:$($_.Name)" }

    Write-Host '=== 0. what is here'
    if ($Owner) {
        $ownerSid = ([Security.Principal.NTAccount]$Owner).Translate([Security.Principal.SecurityIdentifier]).Value
    } else {
        $ownerSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    }
    if ($ownerSid -notmatch '^S-1-5-21-\d+-\d+-\d+-\d+$') { throw "the owner $Owner ($ownerSid) is not a user account; pass -Owner YOUR-ACCOUNT" }
    $ownerName = ([Security.Principal.SecurityIdentifier]$ownerSid).Translate([Security.Principal.NTAccount]).Value
    Write-Host "  owner (lab-run runs as this account): $ownerName $ownerSid"
    $labStates = @(Get-ChildItem -LiteralPath (Join-Path $Data 'labs') -Filter *.json -ErrorAction SilentlyContinue)
    if (-not $labStates) { throw "no lab state in $Data\labs: this install uses an existing lab VM and never builds one" }
    foreach ($file in $labStates) {
        $state = Get-Content -LiteralPath $file.FullName -Raw | ConvertFrom-Json
        $vm = Get-VM -Name $state.vm -ErrorAction SilentlyContinue
        if (-not $vm) { throw "lab state $($file.Name) names VM $($state.vm), which Hyper-V does not have" }
        Write-Host "  lab $($state.vm) (project $($state.project)): $($vm.State)"
    }
    # The guest password's blob is DPAPI LocalMachine: any account on this PC could decrypt it, so the
    # folder's access list is its protection. Nobody but SYSTEM and Administrators may read it.
    $secrets = Join-Path $Data 'secrets'
    if (Test-Path -LiteralPath $secrets) {
        # ReadData, GENERIC_READ or GENERIC_ALL, on the folder or any file in it (review 2026-11-21).
        $readers = @(foreach ($entry in @(Get-Item -LiteralPath $secrets -Force) + @(Get-ChildItem -LiteralPath $secrets -Recurse -Force)) {
                (Get-Acl -LiteralPath $entry.FullName).Access | Where-Object {
                    $_.AccessControlType -eq 'Allow' -and (([long]$_.FileSystemRights -band 0x90000001) -ne 0) -and
                    $Trusted -notcontains $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value
                } | ForEach-Object { "$($_.IdentityReference) on $($entry.Name)" }
            })
        if ($readers) { throw "$secrets can be read by $($readers -join ', '): only SYSTEM and Administrators may read the guest credentials. Fix its access list first" }
        Write-Host "  $secrets is readable by SYSTEM and Administrators only"
    }
    foreach ($tool in $Git, $Go) {
        if (-not (Test-Path -LiteralPath $tool)) { throw "$tool is missing" }
        if ($Trusted -notcontains (Get-OwnerSid $tool)) { throw "$tool is not owned by SYSTEM, Administrators or TrustedInstaller" }
    }

    Write-Host "=== 1. a fresh clone of $Branch"
    Initialize-OwnedDirectory $Data $AdminAndUsersRead -Shallow
    Initialize-OwnedDirectory "$Data\src" $AdminOnly -Shallow
    $cache = "$Data\toolcache"
    Initialize-OwnedDirectory $cache $AdminOnly -Shallow
    New-Item -ItemType Directory -Force "$cache\tmp" | Out-Null
    # Nothing from the profile: a fixed PATH, no global git config (url.insteadOf, core.fsmonitor), no
    # go env file (GOFLAGS=-toolexec), no toolchain download, caches and temp in the admin-only cache.
    $env:PATH = "C:\Windows\System32;C:\Windows;C:\Windows\System32\WindowsPowerShell\v1.0;$(Split-Path $Git);$(Split-Path $Go)"
    $env:GIT_CONFIG_GLOBAL = 'NUL'
    $env:GIT_TERMINAL_PROMPT = '0'
    $env:GOENV = 'off'
    $env:GOTOOLCHAIN = 'local'
    $env:GOFLAGS = '-buildvcs=false'
    $env:GOPROXY = 'off'
    $env:GOCACHE = "$cache\go-build"
    $env:GOMODCACHE = "$cache\go-mod"
    $env:GOPATH = "$cache\go"
    $env:GOTMPDIR = "$cache\tmp"
    $env:TEMP = "$cache\tmp"
    $env:TMP = "$cache\tmp"
    $source = "$Data\src\$(Get-Date -Format 'yyyyMMdd-HHmmss')"
    Invoke-Checked 'git clone' { & $Git clone --quiet --depth 1 --branch $Branch $Repository $source }
    foreach ($entry in @(Get-Item -LiteralPath $source) + @(Get-ChildItem -LiteralPath $source -Recurse -Force)) {
        if ($Trusted -notcontains (Get-OwnerSid $entry.FullName)) { throw "$($entry.FullName) in the fresh clone is not owned by Administrators" }
    }
    $commit = & $Git -c "safe.directory=$($source -replace '\\','/')" -C $source rev-parse HEAD
    Write-Host "  $source at $commit"
    # This script and what it installs must be the same version: a newer script over an older branch
    # would stop the broker, install binaries without the commands it uses, and fail half done.
    if ($PSCommandPath) {
        $cloned = Join-Path $source 'install\install.ps1'
        if (-not (Test-Path -LiteralPath $cloned) -or (Get-FileHash -LiteralPath $PSCommandPath).Hash -ne (Get-FileHash -LiteralPath $cloned).Hash) {
            throw "this script is not $Branch's install\install.ps1: run $Branch's own copy, or pass the -Branch this script came from (nothing has been installed)"
        }
    }

    # Each project's lab exchange is the owner's own folder: lab-run reads and writes it as the owner.
    $ownerProfile = (Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\$ownerSid").ProfileImagePath
    foreach ($file in Get-ChildItem "$source\registry\projects" -Filter *.json) {
        $project = Get-Content -LiteralPath $file.FullName -Raw | ConvertFrom-Json
        if (-not ([string]$project.root).StartsWith($ownerProfile.TrimEnd('\') + '\', [StringComparison]::OrdinalIgnoreCase)) {
            throw "registry\projects\$($file.Name): the lab exchange $($project.root) is not in $ownerName's profile ($ownerProfile)"
        }
    }

    Write-Host '=== 2. vet, suite, build'
    Push-Location $source
    try {
        Invoke-Checked 'go vet' { & $Go vet ./... }
        Invoke-Checked 'the suite' { & $Go test -count=1 ./... }
        foreach ($program in 'lab-broker', 'lab-run') {
            Invoke-Checked "building $program" { & $Go build -trimpath -o "$source\bin\$program.exe" "./cmd/$program" }
        }
    } finally { Pop-Location }

    Write-Host "=== 3. $Programs"
    # Disabled, not only stopped: the task restarts a broker that exits, and one restarted mid-install
    # would keep serving with the old binary and the old client (step 7 registers it enabled again).
    if (Get-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Disable-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName | Out-Null
        Stop-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName
    }
    Stop-InstalledProcess "$Programs\lab-broker.exe"
    New-Item -ItemType Directory -Force $Programs | Out-Null
    if ($Trusted -notcontains (Get-OwnerSid $Programs)) { throw "$Programs is not owned by SYSTEM or Administrators: remove it, then run this again" }
    foreach ($program in 'lab-broker.exe', 'lab-run.exe') { Install-File "$source\bin\$program" "$Programs\$program" }
    # The owner's elevated rebase runs only from here, a copy of the fresh clone's.
    Install-File "$source\install\rebase-os.ps1" "$Programs\rebase-os.ps1"
    Set-Content -LiteralPath "$Programs\installed-commit.txt" -Value "$commit ($Branch)" -Encoding ascii

    Write-Host '=== 4. the registry and the lab exchanges'
    # log\ and staging\ hold argv and copies in transit: SYSTEM and Administrators only.
    Initialize-OwnedDirectory "$Data\log" $AdminOnly
    Initialize-OwnedDirectory "$Data\staging" $AdminOnly
    $projects = "$Data\projects"
    $incoming = "$Data\projects.incoming"
    if (Test-Path -LiteralPath $incoming) { Remove-Item -LiteralPath $incoming -Recurse -Force }
    New-Item -ItemType Directory $incoming | Out-Null
    Copy-Item "$source\registry\projects\*.json" $incoming
    if (Test-Path -LiteralPath $projects) {
        Initialize-OwnedDirectory $projects $AdminAndUsersRead
        Remove-Item -LiteralPath $projects -Recurse -Force
    }
    Move-Item -LiteralPath $incoming -Destination $projects
    Initialize-OwnedDirectory $projects $AdminAndUsersRead
    foreach ($file in Get-ChildItem $projects -Filter *.json) {
        $project = Get-Content -LiteralPath $file.FullName -Raw | ConvertFrom-Json
        foreach ($directory in $project.root, (Join-Path $project.root 'lab-in')) {
            if (-not (Test-Path -LiteralPath $directory)) { New-Item -ItemType Directory -Force $directory | Out-Null }
            if ((Get-Item -LiteralPath $directory -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "$directory is a link or junction" }
        }
        Write-Host "  $($project.name): lab exchange $($project.root) (WSL: $($project.wsl_root))"
    }

    Write-Host "=== 5. the labs' client: $ownerName"
    # The lab states cannot be made again (the VM was built once): keep a copy before any changes.
    $changing = @($labStates | Where-Object { (Get-Content -LiteralPath $_.FullName -Raw | ConvertFrom-Json).client_sid -ne $ownerSid })
    if ($changing) {
        $backup = "$Data\labs.before-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
        Copy-Item -LiteralPath "$Data\labs" -Destination $backup -Recurse
        Write-Host "  the lab states before this change: $backup"
    }
    Invoke-Checked 'lab-broker set-owner' { & "$Programs\lab-broker.exe" set-owner $ownerSid }

    Write-Host "=== 6. the host's Enhanced Session mode off"
    Set-VMHost -EnableEnhancedSessionMode $false

    Write-Host "=== 7. the broker task ($TaskPath$TaskName, SYSTEM, at startup)"
    $action = New-ScheduledTaskAction -Execute "$Programs\lab-broker.exe" -Argument 'serve'
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
        -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    $principal = New-ScheduledTaskPrincipal -UserId 'S-1-5-18' -LogonType ServiceAccount -RunLevel Highest
    Stop-InstalledProcess "$Programs\lab-broker.exe" 5
    Register-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force | Out-Null
    Enable-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName | Out-Null
    Start-ScheduledTask -TaskPath $TaskPath -TaskName $TaskName
    $deadline = (Get-Date).AddSeconds(30)
    while (-not (Test-PipeListening) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
    if (-not (Test-PipeListening)) { throw "the broker did not start listening on \\.\pipe\$Pipe within 30 seconds; to see why, stop the task (Stop-ScheduledTask -TaskPath '$TaskPath' -TaskName $TaskName) and run & '$Programs\lab-broker.exe' serve here" }

    Write-Host '=== 8. checks'
    Write-Host '--- lab-broker check'
    Invoke-Checked 'lab-broker check' { & "$Programs\lab-broker.exe" check }
    $refused = @()
    foreach ($file in $labStates) {
        $state = Get-Content -LiteralPath $file.FullName -Raw | ConvertFrom-Json
        Write-Host "--- lab-run status ($($state.vm)): the gate against the real VM"
        & "$Programs\lab-run.exe" --project $state.project --vm $state.vm status
        if ($LASTEXITCODE -ne 0) { $refused += $state.vm }
    }
    if ($refused) { throw "the gate REFUSED $($refused -join ', '): read the lines above before using them" }

    # Earlier clones are only build trees; the one just built stays until the next install.
    Get-ChildItem -LiteralPath "$Data\src" -Directory | Where-Object { $_.FullName -ne $source } |
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host ''
    Write-Host "Installed $commit ($Branch). In WSL, install (or reinstall) the WSL side so /usr/local/bin/lab-run exists:" -ForegroundColor Green
    Write-Host '  sudo bash wsl/install.sh   (from a fresh clone, in WSL)'
    Write-Host 'Then, in WSL (unelevated, the way agents call it, which this elevated check is not): lab-run status'
    exit 0
} catch {
    Write-Host "FAILED: $_" -ForegroundColor Red
    exit 1
}
