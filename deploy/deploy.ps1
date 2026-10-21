#Requires -Version 7.0
<#
.SYNOPSIS
    Build bento-ads and deploy it to a Windows target over SSH.
.DESCRIPTION
    Logs in to the target, creates its RemoteDir if needed, compares the
    running version/files against what we'd push, and -- after
    confirmation -- copies the binary and its config/env/cert files,
    installs the service if missing or upgrades it if present, and starts
    it. See deploy/README.md for the target file format and the flow.
.PARAMETER Target
    Name of a deploy/targets/<Target>.psd1 file (without the extension).
.EXAMPLE
    .\deploy\deploy.ps1 machine-01
.EXAMPLE
    .\deploy\deploy.ps1 machine-01 -Yes
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Target,

    # Build from an uncommitted tree anyway (normally refused: the pushed
    # binary's own --version would otherwise be the only record of exactly
    # what's running).
    [switch]$AllowDirty,

    # Skip the "remote is X, we are pushing Y, continue?" and rollback
    # prompts.
    [switch]$Yes,

    # Proceed even if the remote version and every file already match.
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

# Backup directories to keep under RemoteDir\backup\ (oldest deleted first).
$script:KeepBackups = 5

# How long Apply waits for the old process to exit after Stop-Service, and
# Verify waits for the new one to be confirmed running, in seconds.
$script:StopTimeoutSec = 45
$script:VerifyTimeoutSec = 20

#region Target loading --------------------------------------------------------

function Get-DeployTarget {
    <#
    Loads deploy/targets/<Name>.psd1, applies defaults, and resolves its
    local file paths against the repo root so the rest of the script can
    use them without caring where it was invoked from.
    #>
    param([Parameter(Mandatory)][string]$Name)

    $path = Join-Path $RepoRoot "deploy/targets/$Name.psd1"
    if (-not (Test-Path $path)) {
        throw "No such target file: $path"
    }
    $data = Import-PowerShellDataFile -Path $path

    foreach ($required in 'Host', 'User', 'KeyFile', 'ConfigFile', 'EnvFile') {
        if (-not $data.ContainsKey($required) -or [string]::IsNullOrWhiteSpace($data[$required])) {
            throw "Target '$Name' is missing required field '$required'"
        }
    }

    if (-not $data.ContainsKey('RemoteDir') -or [string]::IsNullOrWhiteSpace($data.RemoteDir)) {
        $data.RemoteDir = 'C:\bento'
    }
    if (-not $data.ContainsKey('CertFiles')) {
        $data.CertFiles = @()
    }
    if (-not $data.ContainsKey('ServiceName') -or [string]::IsNullOrWhiteSpace($data.ServiceName)) {
        $data.ServiceName = 'bento-ads'
    }
    if (-not $data.ContainsKey('ServiceRecovery')) {
        $data.ServiceRecovery = @{}
    }
    $recoveryDefaults = @{RestartDelaySec1 = 60; RestartDelaySec2 = 60; RestartDelaySec3 = 300; ResetDays = 1 }
    foreach ($kv in $recoveryDefaults.GetEnumerator()) {
        if (-not $data.ServiceRecovery.ContainsKey($kv.Key)) { $data.ServiceRecovery[$kv.Key] = $kv.Value }
    }

    # KeyFile commonly starts with ~; ssh/scp handle that themselves, but
    # Test-Path below needs it expanded.
    $keyFileExpanded = $data.KeyFile -replace '^~', $HOME
    if (-not (Test-Path $keyFileExpanded)) {
        throw "Target '$Name': KeyFile not found: $($data.KeyFile)"
    }

    $data.ConfigFile = (Resolve-Path (Join-Path $RepoRoot $data.ConfigFile)).Path
    $data.EnvFile = (Resolve-Path (Join-Path $RepoRoot $data.EnvFile)).Path
    $data.CertFiles = @($data.CertFiles | ForEach-Object { (Resolve-Path (Join-Path $RepoRoot $_)).Path })

    return [PSCustomObject]$data
}

#endregion

#region Remote scriptblocks ---------------------------------------------------
# Each block takes one $Params argument (a Hashtable when called directly --
# see deploy/test-remote-blocks.ps1 -- or the PSCustomObject that
# Invoke-RemoteBlock gets back from ConvertFrom-Json on the remote side;
# property access with "." works on both) and returns one PSCustomObject.
#
# These run on Windows PowerShell 5.1 on the target (Invoke-RemoteBlock
# ships them there via -EncodedCommand), so: no ternaries, no ??, no
# -Parallel, no classes -- only syntax 5.1 understands. They also can't
# call any function defined elsewhere in this file, since only the block's
# own text is shipped; anything a block needs is written out inline.

# Test-RemoteInventoryBlock: read-only. Reports what's on the target today
# so Main can decide what, if anything, needs to change.
$script:InventoryBlock = {
    param($Params)

    $remoteDir = $Params.RemoteDir
    $serviceName = $Params.ServiceName
    $exePath = Join-Path $remoteDir 'bento-ads.exe'

    $result = @{
        RemoteDirExists   = Test-Path $remoteDir
        ExeExists         = Test-Path $exePath
        VersionOutput     = $null
        ExeSha256         = $null
        ServiceExists     = $false
        ServiceStatus     = $null
        ServicePathName   = $null
        ServiceProcessId  = $null
        FileHashes        = @{}
        ConsoleProcessIds = @()
    }

    if ($result.ExeExists) {
        try {
            $out = & $exePath '--version' 2>&1
            $result.VersionOutput = ($out -join "`n").Trim()
        } catch {
            $result.VersionOutput = $null
        }
        if (-not ($result.VersionOutput -and $result.VersionOutput.StartsWith('bento-ads '))) {
            # Pre-Part-A build, or --version failed outright: hash the exe
            # so Main can report "legacy (<sha256 prefix>)" instead of a
            # version it can't read.
            $result.ExeSha256 = (Get-FileHash -Path $exePath -Algorithm SHA256).Hash
        }
    }

    $svc = Get-CimInstance -ClassName Win32_Service -Filter "Name='$serviceName'" -ErrorAction SilentlyContinue
    if ($svc) {
        $result.ServiceExists = $true
        $result.ServiceStatus = $svc.State
        $result.ServicePathName = $svc.PathName
        if ($svc.ProcessId -and $svc.ProcessId -gt 0) {
            $result.ServiceProcessId = $svc.ProcessId
        }
    }

    # Named files deployed next to the exe: config under its own name, the
    # env file always as ".env", each cert under its own name in certs\.
    $filesToHash = @{}
    if ($Params.ConfigFileName) { $filesToHash[$Params.ConfigFileName] = Join-Path $remoteDir $Params.ConfigFileName }
    $filesToHash['.env'] = Join-Path $remoteDir '.env'
    foreach ($certName in $Params.CertFileNames) {
        $filesToHash[$certName] = Join-Path $remoteDir (Join-Path 'certs' $certName)
    }
    foreach ($name in $filesToHash.Keys) {
        $p = $filesToHash[$name]
        if (Test-Path $p) {
            $result.FileHashes[$name] = (Get-FileHash -Path $p -Algorithm SHA256).Hash
        } else {
            $result.FileHashes[$name] = $null
        }
    }

    # A bento-ads.exe process NOT owned by the service would fight it for
    # the MQTT client_id -- Main aborts outright if any turn up.
    $procs = Get-CimInstance -ClassName Win32_Process -Filter "Name='bento-ads.exe'" -ErrorAction SilentlyContinue
    foreach ($proc in $procs) {
        if (-not $result.ServiceProcessId -or $proc.ProcessId -ne $result.ServiceProcessId) {
            $result.ConsoleProcessIds += $proc.ProcessId
        }
    }

    [PSCustomObject]$result
}

# PrepareDirsBlock: creates the directories staging needs. Separate from
# Inventory so it's never run for a dry inventory-only check.
$script:PrepareDirsBlock = {
    param($Params)

    $remoteDir = $Params.RemoteDir
    $dirs = @(
        $remoteDir
        (Join-Path $remoteDir 'logs')
        (Join-Path $remoteDir 'certs')
        (Join-Path $remoteDir 'staging')
        (Join-Path $remoteDir 'backup')
    )
    $created = @()
    foreach ($d in $dirs) {
        if (-not (Test-Path $d)) {
            New-Item -ItemType Directory -Path $d -Force | Out-Null
            $created += $d
        }
    }

    [PSCustomObject]@{ Created = $created }
}

# VerifyStagedBlock: after scp, confirms what landed in staging\ hashes to
# what Main expected to send -- catches a truncated/corrupted transfer
# before Apply touches anything live.
$script:VerifyStagedBlock = {
    param($Params)

    $stagingDir = Join-Path $Params.RemoteDir 'staging'
    $mismatches = @()
    $missing = @()
    # ExpectedHashes is a Hashtable when called directly (its keys are its
    # own .Keys, not .PSObject.Properties) or a PSCustomObject when it came
    # back off the wire via ConvertFrom-Json (the reverse); -is picks the
    # right way to list its keys either way.
    if ($Params.ExpectedHashes -is [System.Collections.IDictionary]) {
        $expectedNames = @($Params.ExpectedHashes.Keys)
    } else {
        $expectedNames = @($Params.ExpectedHashes.PSObject.Properties.Name)
    }
    foreach ($name in $expectedNames) {
        $expected = $Params.ExpectedHashes.$name
        $p = Join-Path $stagingDir $name
        if (-not (Test-Path $p)) {
            $missing += $name
            continue
        }
        $actual = (Get-FileHash -Path $p -Algorithm SHA256).Hash
        if ($actual -ne $expected) {
            $mismatches += $name
        }
    }

    $ok = (($mismatches.Count -eq 0) -and ($missing.Count -eq 0))
    if ($ok) {
        # staging\ otherwise sits world-readable (inherited ACLs) until
        # Apply moves .env into place -- lock it here too, the moment its
        # hash is confirmed.
        $stagedEnvPath = Join-Path $stagingDir '.env'
        if (Test-Path $stagedEnvPath) {
            icacls $stagedEnvPath /inheritance:r | Out-Null
            icacls $stagedEnvPath /grant:r '*S-1-5-32-544:F' '*S-1-5-18:F' | Out-Null
        }
    }

    [PSCustomObject]@{
        Ok         = $ok
        Mismatches = $mismatches
        Missing    = $missing
    }
}

# ApplyBlock: stops the old process, backs it up, moves the staged files
# into place, installs/upgrades the service as needed, and starts it.
# NeedsReinstall is decided by Main (it needs the tolerant PathName
# comparison, which lives there so it isn't duplicated in block text).
$script:ApplyBlock = {
    param($Params)

    $remoteDir = $Params.RemoteDir
    $serviceName = $Params.ServiceName
    $exePath = Join-Path $remoteDir 'bento-ads.exe'
    $configPath = Join-Path $remoteDir $Params.ConfigFileName
    $envPath = Join-Path $remoteDir '.env'
    $stagingDir = Join-Path $remoteDir 'staging'
    $backupRoot = Join-Path $remoteDir 'backup'

    # Narrated as it goes, for Main to print -- the useful record of what
    # Apply actually did lives on the target, in the order it happened.
    $steps = New-Object 'System.Collections.Generic.List[string]'

    $existed = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
    if ($existed) {
        $stopStart = Get-Date
        Stop-Service -Name $serviceName -Force -ErrorAction SilentlyContinue
        $deadline = $stopStart.AddSeconds($Params.StopTimeoutSec)
        $exited = $false
        while ((Get-Date) -lt $deadline) {
            if (-not (Get-Process -Name 'bento-ads' -ErrorAction SilentlyContinue)) {
                $exited = $true
                break
            }
            Start-Sleep -Seconds 1
        }
        $waitedSec = [int]((Get-Date) - $stopStart).TotalSeconds
        if (-not $exited) {
            throw "bento-ads.exe did not exit within $($Params.StopTimeoutSec)s of Stop-Service"
        }
        $steps.Add("stopped the service (waited ${waitedSec}s for the process to exit)")
    } else {
        $steps.Add('no existing service to stop')
    }

    # Locks a .env copy (live, staged or backed up) to Administrators+SYSTEM
    # only, by well-known SID so this doesn't depend on OS locale. Called at
    # every point a .env lands on disk -- the backup copy inherits the
    # live file's old (possibly looser) ACL otherwise, and Copy-Item/
    # Move-Item don't carry a source's explicit ACL across on their own.
    function Protect-EnvFile {
        param($Path)
        if (Test-Path $Path) {
            icacls $Path /inheritance:r | Out-Null
            icacls $Path /grant:r '*S-1-5-32-544:F' '*S-1-5-18:F' | Out-Null
        }
    }

    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $backupDir = Join-Path $backupRoot $stamp
    $certBackupDir = Join-Path $backupDir 'certs'
    New-Item -ItemType Directory -Path $backupDir -Force | Out-Null

    try {
        foreach ($leaf in @('bento-ads.exe', $Params.ConfigFileName, '.env')) {
            $src = Join-Path $remoteDir $leaf
            if (Test-Path $src) {
                Copy-Item -Path $src -Destination (Join-Path $backupDir $leaf) -Force
            }
        }
        Protect-EnvFile (Join-Path $backupDir '.env')
        $steps.Add("backed up current files to $backupDir")

        foreach ($certName in $Params.CertFileNames) {
            $src = Join-Path $remoteDir (Join-Path 'certs' $certName)
            if (Test-Path $src) {
                if (-not (Test-Path $certBackupDir)) { New-Item -ItemType Directory -Path $certBackupDir -Force | Out-Null }
                Copy-Item -Path $src -Destination (Join-Path $certBackupDir $certName) -Force
            }
        }

        foreach ($leaf in $Params.StagedFileNames) {
            $src = Join-Path $stagingDir $leaf
            if (-not (Test-Path $src)) { continue }
            if ($Params.CertFileNames -contains $leaf) {
                $dst = Join-Path $remoteDir (Join-Path 'certs' $leaf)
            } elseif ($leaf -eq '.env') {
                $dst = $envPath
            } elseif ($leaf -eq 'bento-ads.exe') {
                $dst = $exePath
            } else {
                $dst = Join-Path $remoteDir $leaf
            }
            Move-Item -Path $src -Destination $dst -Force
        }
        Protect-EnvFile $envPath
        if ($Params.StagedFileNames -and @($Params.StagedFileNames).Count -gt 0) {
            $steps.Add("swapped in: $(@($Params.StagedFileNames) -join ', ')")
        } else {
            $steps.Add('no files to swap')
        }

        if ($Params.NeedsReinstall) {
            if ($existed) {
                & $exePath service uninstall 2>&1 | Out-Null
            }
            & $exePath service install -c $configPath -e $envPath 2>&1 | Out-Null
            if ($LASTEXITCODE -ne 0) {
                throw "bento-ads.exe service install exited $LASTEXITCODE"
            }
            $steps.Add('reinstalled the service')

            $rec = $Params.ServiceRecovery
            $actions = "restart/$($rec.RestartDelaySec1 * 1000)/restart/$($rec.RestartDelaySec2 * 1000)/restart/$($rec.RestartDelaySec3 * 1000)"
            $resetSec = $rec.ResetDays * 86400
            sc.exe failure $serviceName reset= $resetSec actions= $actions | Out-Null
            $steps.Add('set the recovery policy')
        } else {
            $steps.Add('kept the existing service installation')
        }

        $applyTimeUtc = (Get-Date).ToUniversalTime().ToString('o')
        Start-Service -Name $serviceName
        $steps.Add('started the service')

        # Trim old backups, oldest first, keeping the KeepBackups most recent.
        $allBackups = Get-ChildItem -Path $backupRoot -Directory | Sort-Object Name -Descending
        $toDelete = @($allBackups | Select-Object -Skip $Params.KeepBackups)
        foreach ($old in $toDelete) {
            Remove-Item -Path $old.FullName -Recurse -Force -ErrorAction SilentlyContinue
        }
    } catch {
        # The service is stopped (or was never running) at this point, and
        # whatever got this far may have half-swapped live files. Put back
        # whatever this attempt managed to back up, re-lock .env, and try
        # to get the service running again before reporting the failure.
        $applyError = $_.Exception.Message
        $restoreNote = 'restored the previous build'
        try {
            foreach ($leaf in @('bento-ads.exe', $Params.ConfigFileName, '.env')) {
                $backupFile = Join-Path $backupDir $leaf
                if (Test-Path $backupFile) {
                    Copy-Item -Path $backupFile -Destination (Join-Path $remoteDir $leaf) -Force
                }
            }
            Protect-EnvFile $envPath
            if (Test-Path $certBackupDir) {
                Get-ChildItem -Path $certBackupDir -File | ForEach-Object {
                    Copy-Item -Path $_.FullName -Destination (Join-Path $remoteDir (Join-Path 'certs' $_.Name)) -Force
                }
            }

            $svcNow = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
            if ($svcNow) {
                Start-Service -Name $serviceName -ErrorAction Stop
                $restoreNote += ' and restarted it'
            } else {
                $restoreNote += ', but no service exists to restart it (needs `service install` by hand)'
            }
        } catch {
            $restoreNote = "tried to restore and restart, but that failed too: $($_.Exception.Message)"
        }
        throw "Apply failed ($applyError); $restoreNote. Steps completed before the failure: $(@($steps) -join '; ')."
    }

    [PSCustomObject]@{
        BackupDir     = $backupDir
        Reinstalled   = [bool]$Params.NeedsReinstall
        ApplyTimeUtc  = $applyTimeUtc
        BackupsPruned = $toDelete.Count
        Steps         = @($steps)
    }
}

# VerifyBlock: confirms the service is Running on a process that started
# after Apply, that it logged a "starting" line for the version we pushed,
# and that no bento-ads ERROR events have appeared since.
$script:VerifyBlock = {
    param($Params)

    $serviceName = $Params.ServiceName
    $sinceUtc = [datetime]::Parse($Params.SinceUtc).ToUniversalTime()
    $deadline = (Get-Date).AddSeconds($Params.TimeoutSec)

    $running = $false
    $processStartTime = $null
    while ((Get-Date) -lt $deadline) {
        $svc = Get-CimInstance -ClassName Win32_Service -Filter "Name='$serviceName'" -ErrorAction SilentlyContinue
        if ($svc -and $svc.State -eq 'Running' -and $svc.ProcessId -gt 0) {
            $proc = Get-CimInstance -ClassName Win32_Process -Filter "ProcessId=$($svc.ProcessId)" -ErrorAction SilentlyContinue
            if ($proc) {
                $processStartTime = [Management.ManagementDateTimeConverter]::ToDateTime($proc.CreationDate).ToUniversalTime()
                if ($processStartTime -ge $sinceUtc.AddSeconds(-2)) {
                    $running = $true
                    break
                }
            }
        }
        Start-Sleep -Milliseconds 500
    }

    $serviceProcessId = $null
    if ($running) {
        $svcNow = Get-CimInstance -ClassName Win32_Service -Filter "Name='$serviceName'" -ErrorAction SilentlyContinue
        if ($svcNow -and $svcNow.ProcessId -gt 0) { $serviceProcessId = $svcNow.ProcessId }
    }

    # -ErrorAction SilentlyContinue + a $null check, not -ErrorAction Stop
    # + try/catch: Get-WinEvent's own "no matching events" non-terminating
    # error, even suppressed, could still land on a stream -ErrorAction
    # doesn't touch (Warning/Information) and show up as stray text after
    # this block's JSON result on the wire. SilentlyContinue swallows it at
    # the source instead of relying on catching it after the fact.
    $foundStartEvent = $false
    $startEventError = $null
    $events = Get-WinEvent -FilterHashtable @{LogName = 'Application'; ProviderName = 'bento-ads'; StartTime = $sinceUtc.AddSeconds(-2) } -ErrorAction SilentlyContinue
    if ($events) {
        foreach ($e in $events) {
            if ($e.Message -like '*bento-ads service starting*' -and $e.Message -like "*version=$($Params.ExpectedVersion)*") {
                $foundStartEvent = $true
                break
            }
        }
    } else {
        $startEventError = "no matching Application-log events from provider 'bento-ads' since $($sinceUtc.ToString('o'))"
    }

    $errorEvents = @()
    $errs = Get-WinEvent -FilterHashtable @{LogName = 'Application'; ProviderName = 'bento-ads'; Level = 2; StartTime = $sinceUtc.AddSeconds(-2) } -ErrorAction SilentlyContinue
    if ($errs) {
        foreach ($e in $errs) { $errorEvents += $e.Message }
    }

    [PSCustomObject]@{
        ServiceRunning    = $running
        ServiceProcessId  = $serviceProcessId
        ProcessStartUtc   = $processStartTime
        FoundStartEvent   = $foundStartEvent
        StartEventError   = $startEventError
        ErrorEventsFound  = $errorEvents
    }
}

# RollbackBlock: restores a backup directory over the live files and
# restarts the service, for Main to call when Verify fails.
$script:RollbackBlock = {
    param($Params)

    $remoteDir = $Params.RemoteDir
    $serviceName = $Params.ServiceName
    $backupDir = $Params.BackupDir
    $steps = New-Object 'System.Collections.Generic.List[string]'

    $stopStart = Get-Date
    Stop-Service -Name $serviceName -Force -ErrorAction SilentlyContinue
    $deadline = $stopStart.AddSeconds($Params.StopTimeoutSec)
    while ((Get-Date) -lt $deadline) {
        if (-not (Get-Process -Name 'bento-ads' -ErrorAction SilentlyContinue)) { break }
        Start-Sleep -Seconds 1
    }
    $waitedSec = [int]((Get-Date) - $stopStart).TotalSeconds
    $steps.Add("stopped the service (waited ${waitedSec}s for the process to exit)")

    Get-ChildItem -Path $backupDir -File | ForEach-Object {
        Copy-Item -Path $_.FullName -Destination (Join-Path $remoteDir $_.Name) -Force
    }
    # Copy-Item doesn't carry the backup copy's locked-down ACL across;
    # re-lock the restored live .env the same way Apply does.
    $restoredEnvPath = Join-Path $remoteDir '.env'
    if (Test-Path $restoredEnvPath) {
        icacls $restoredEnvPath /inheritance:r | Out-Null
        icacls $restoredEnvPath /grant:r '*S-1-5-32-544:F' '*S-1-5-18:F' | Out-Null
    }
    $certBackupDir = Join-Path $backupDir 'certs'
    if (Test-Path $certBackupDir) {
        Get-ChildItem -Path $certBackupDir -File | ForEach-Object {
            Copy-Item -Path $_.FullName -Destination (Join-Path $remoteDir (Join-Path 'certs' $_.Name)) -Force
        }
    }
    $steps.Add("restored files from $backupDir")

    $restartTimeUtc = (Get-Date).ToUniversalTime().ToString('o')
    Start-Service -Name $serviceName
    $steps.Add('started the service')

    [PSCustomObject]@{ RestartTimeUtc = $restartTimeUtc; Steps = @($steps) }
}

# UninstallBlock: not part of the deploy flow -- used by
# deploy/test-remote-blocks.ps1 to tear its scenarios down.
$script:UninstallBlock = {
    param($Params)

    $serviceName = $Params.ServiceName
    $svc = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
    if ($svc) {
        Stop-Service -Name $serviceName -Force -ErrorAction SilentlyContinue
        $exePath = Join-Path $Params.RemoteDir 'bento-ads.exe'
        if (Test-Path $exePath) {
            & $exePath service uninstall 2>&1 | Out-Null
        }
    }

    [PSCustomObject]@{ Removed = [bool]$svc }
}

#endregion

#region Remote transport -------------------------------------------------------

function ConvertFrom-CliXmlNoise {
    <#
    A non-interactive remote PowerShell has no real console, so anything it
    writes to the error/warning/verbose/debug/information streams can come
    back serialized as "#< CLIXML" + an <Objs>...</Objs> block instead of
    plain text. Strips those blocks out, keeping any genuine plain-text
    lines around them, so what's left is fit to show as a warning.
    #>
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return '' }

    $lines = $Text -split "`r?`n"
    $kept = New-Object 'System.Collections.Generic.List[string]'
    $inObjs = $false
    foreach ($line in $lines) {
        if ($line -match '^#<\s*CLIXML') { $inObjs = $false; continue }
        if ($line -match '^\s*<Objs\b') { $inObjs = $true }
        if ($inObjs) {
            if ($line -match '</Objs>\s*$') { $inObjs = $false }
            continue
        }
        if ($line.Trim() -ne '') { $kept.Add($line) }
    }
    return ($kept -join "`n")
}

function New-RemoteBlockScript {
    <#
    Builds the text of the script a block runs as, remotely: preference
    variables that stop stray stream noise at the source (see VerifyBlock's
    Get-WinEvent calls for why that alone isn't enough), params in as JSON,
    the block's own text embedded verbatim, and the result written as a
    single compact JSON line right after a sentinel line and nothing else.
    Factored out of Invoke-RemoteBlock so the exact same script can be run
    locally through powershell.exe -EncodedCommand too, with no ssh needed
    -- see deploy/test-remote-blocks.ps1's transport-parsing scenario.
    #>
    param(
        [Parameter(Mandatory)][scriptblock]$Block,
        [hashtable]$BlockParams = @{},
        [string]$Sentinel = '___DEPLOY_RESULT___'
    )

    $paramsJson = $BlockParams | ConvertTo-Json -Depth 10 -Compress
    return @"
`$ProgressPreference = 'SilentlyContinue'
`$WarningPreference = 'SilentlyContinue'
`$VerbosePreference = 'SilentlyContinue'
`$InformationPreference = 'SilentlyContinue'
`$blockParams = ConvertFrom-Json @'
$paramsJson
'@
`$block = {
$($Block.ToString())
}
`$result = & `$block `$blockParams
Write-Output '$Sentinel'
Write-Output (ConvertTo-Json -InputObject `$result -Depth 10 -Compress)
"@
}

function ConvertFrom-RemoteBlockOutput {
    <#
    Finds the sentinel in a block of stdout lines and parses exactly the
    one line after it as JSON -- never anything past that, so trailing
    stray text (see New-RemoteBlockScript) is never even looked at.
    #>
    param(
        [AllowEmptyCollection()][array]$StdOut = @(),
        [string]$Sentinel = '___DEPLOY_RESULT___'
    )

    $idx = -1
    for ($i = 0; $i -lt $StdOut.Count; $i++) {
        if ($StdOut[$i] -eq $Sentinel) { $idx = $i; break }
    }
    if ($idx -lt 0 -or ($idx + 1) -ge $StdOut.Count) {
        throw "no result line after the sentinel in output:`n$($StdOut -join "`n")"
    }
    $jsonLine = $StdOut[$idx + 1]
    if ([string]::IsNullOrWhiteSpace($jsonLine)) {
        return $null
    }
    return $jsonLine | ConvertFrom-Json
}

function Invoke-RemoteBlock {
    <#
    Runs a scriptblock on $TargetCfg over ssh (New-RemoteBlockScript builds
    what actually runs there, ConvertFrom-RemoteBlockOutput parses what
    comes back). stdout and stderr are captured separately (stderr to a
    temp file, never merged into stdout): any non-empty, de-noised stderr
    (ConvertFrom-CliXmlNoise) is shown as a warning rather than silently
    dropped or risking corruption of the result. One ssh connection per
    call.
    #>
    param(
        [Parameter(Mandatory)][scriptblock]$Block,
        [Parameter(Mandatory)]$TargetCfg,
        [hashtable]$BlockParams = @{}
    )

    $sentinel = '___DEPLOY_RESULT___'
    $remoteScript = New-RemoteBlockScript -Block $Block -BlockParams $BlockParams -Sentinel $sentinel

    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($remoteScript))
    $keyFile = $TargetCfg.KeyFile
    $dest = "$($TargetCfg.User)@$($TargetCfg.Host)"
    $remoteCmd = "powershell -NoProfile -NonInteractive -EncodedCommand $encoded"
    $sshArgs = @('-i', $keyFile, '-o', 'IdentitiesOnly=yes', $dest, $remoteCmd)
    Write-Verbose "ssh -i $keyFile -o IdentitiesOnly=yes $dest `"powershell -NoProfile -NonInteractive -EncodedCommand <$($encoded.Length) bytes>`""

    $stderrFile = Join-Path ([System.IO.Path]::GetTempPath()) ("deploy-stderr-" + [guid]::NewGuid() + '.txt')
    try {
        $stdout = @(& ssh @sshArgs 2>$stderrFile)
        $exitCode = $LASTEXITCODE
        $stderrRaw = if (Test-Path $stderrFile) { Get-Content -Path $stderrFile -Raw } else { '' }
    } finally {
        Remove-Item -Path $stderrFile -ErrorAction SilentlyContinue
    }

    $stderrClean = ConvertFrom-CliXmlNoise $stderrRaw
    if ($stderrClean) {
        Write-Host $stderrClean -ForegroundColor Yellow
    }

    if ($exitCode -ne 0) {
        throw "ssh to $dest failed (exit $exitCode): $($stdout -join "`n")$(if ($stderrClean) { "`n$stderrClean" })"
    }

    return ConvertFrom-RemoteBlockOutput -StdOut $stdout -Sentinel $sentinel
}

function Invoke-RemoteCommand {
    <# Runs a plain command string over ssh, for one-liners not worth a block. #>
    param([Parameter(Mandatory)]$TargetCfg, [Parameter(Mandatory)][string]$Command)

    $dest = "$($TargetCfg.User)@$($TargetCfg.Host)"
    Write-Verbose "ssh -i $($TargetCfg.KeyFile) -o IdentitiesOnly=yes $dest `"$Command`""
    $output = & ssh -i $TargetCfg.KeyFile -o IdentitiesOnly=yes $dest $Command 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "ssh to $dest failed (exit $LASTEXITCODE): $($output -join "`n")"
    }
    return $output
}

function Send-StagedFile {
    param([Parameter(Mandatory)]$TargetCfg, [Parameter(Mandatory)][string]$LocalPath, [Parameter(Mandatory)][string]$RemoteName)

    $dest = "$($TargetCfg.User)@$($TargetCfg.Host)"
    # scp against this Windows OpenSSH server wants forward slashes in the
    # remote path (backslashes get mangled); RemoteDir stays backslash-form
    # everywhere else, so just swap them here.
    $remoteDirFwd = $TargetCfg.RemoteDir -replace '\\', '/'
    $remoteStaging = "$remoteDirFwd/staging/$RemoteName"
    Write-Verbose "scp -i $($TargetCfg.KeyFile) -o IdentitiesOnly=yes $LocalPath `"${dest}:${remoteStaging}`""
    # Let scp show its own progress on the console (no output redirection);
    # the exit code alone tells us whether it worked.
    & scp -i $TargetCfg.KeyFile -o IdentitiesOnly=yes $LocalPath "${dest}:${remoteStaging}"
    if ($LASTEXITCODE -ne 0) {
        throw "scp of $LocalPath to $dest failed (exit $LASTEXITCODE)"
    }
}

#endregion

#region Comparison helpers (local, PS7) ---------------------------------------

function Test-PathNameMatches {
    <# Tolerant compare: Windows service PathName vs. what we'd install --
    quoting and whitespace vary by kardianos/service version and by
    whether a path happens to contain a space, neither of which is a real
    difference. #>
    param([string]$Actual, [string]$Expected)
    if ([string]::IsNullOrWhiteSpace($Actual)) { return $false }
    $norm = {
        param($s)
        (($s -replace '"', '') -replace '\s+', ' ').Trim()
    }
    return (& $norm $Actual) -eq (& $norm $Expected)
}

function Get-VersionTag {
    <# Bare module version out of the "bento-ads v0.1.0 (...)" --version
    line -- what program.go logs to the Event Log on service start. #>
    param([string]$VersionOutput)
    if ($VersionOutput -match '^bento-ads (\S+)') {
        return $Matches[1]
    }
    return $null
}

#endregion

#region Progress ----------------------------------------------------------------
# Main's six steps, numbered and timed, so a run against a real target reads
# as progress rather than a long silence followed by either "OK" or a wall
# of ssh error text.

$script:TotalSteps = 6
$script:StepNum = 0
$script:StepSw = $null

function Start-Step {
    param([Parameter(Mandatory)][string]$Title)
    $script:StepNum++
    Write-Host "[$script:StepNum/$script:TotalSteps] $Title"
    $script:StepSw = [Diagnostics.Stopwatch]::StartNew()
}

function Stop-Step {
    Write-Host ("  done in {0:N1}s" -f $script:StepSw.Elapsed.TotalSeconds)
}

function Write-VerifyResult {
    <# Prints what Verify actually checked, not just whether it passed --
    called for both the initial verify and any re-verify after a
    rollback. #>
    param([Parameter(Mandatory)]$Verify)

    if ($Verify.ServiceRunning) {
        Write-Host "  checked: service running (pid=$($Verify.ServiceProcessId), started $($Verify.ProcessStartUtc))"
    } else {
        Write-Host "  checked: service not confirmed running"
    }
    if ($Verify.FoundStartEvent) {
        Write-Host "  checked: found a 'starting' Event Log entry for the expected version"
    } else {
        Write-Host "  checked: no matching 'starting' Event Log entry found$(if ($Verify.StartEventError) { " ($($Verify.StartEventError))" })"
    }
    Write-Host "  checked: $(@($Verify.ErrorEventsFound).Count) ERROR event(s) logged since start"
}

#endregion

#region Main --------------------------------------------------------------------

function Main {
    if (-not $Target) {
        throw "Usage: .\deploy\deploy.ps1 <target> [-AllowDirty] [-Yes] [-Force]"
    }

    $cfg = Get-DeployTarget -Name $Target
    $configName = Split-Path $cfg.ConfigFile -Leaf
    $certNames = @($cfg.CertFiles | ForEach-Object { Split-Path $_ -Leaf })

    Start-Step 'Build'
    $dirty = (git -C $RepoRoot status --porcelain)
    if ($dirty -and -not $AllowDirty) {
        throw "Working tree is dirty; commit or pass -AllowDirty.`n$dirty"
    }

    # No caching: every deploy builds fresh, so --version always reflects
    # exactly what's in the tree right now.
    Write-Host "  building bento-ads.exe..."
    $buildDir = Join-Path ([System.IO.Path]::GetTempPath()) ("bento-ads-deploy-" + [guid]::NewGuid())
    New-Item -ItemType Directory -Path $buildDir -Force | Out-Null
    $localExe = Join-Path $buildDir 'bento-ads.exe'
    try {
        Push-Location $RepoRoot
        $env:GOOS = 'windows'
        $env:GOARCH = 'amd64'
        $env:CGO_ENABLED = '0'
        if ($VerbosePreference -eq 'Continue') {
            go build -v -trimpath -o $localExe ./cmd/bento-ads
        } else {
            go build -trimpath -o $localExe ./cmd/bento-ads
        }
        if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)" }
    } finally {
        Pop-Location
        Remove-Item Env:\GOOS, Env:\GOARCH, Env:\CGO_ENABLED -ErrorAction SilentlyContinue
    }
    $yFull = (& $localExe --version).Trim()
    $yTag = Get-VersionTag $yFull
    Write-Host "  built: $yFull"
    Stop-Step

    Start-Step "Inventory $($cfg.Host)"
    $inv = Invoke-RemoteBlock -Block $script:InventoryBlock -TargetCfg $cfg -BlockParams @{
        RemoteDir      = $cfg.RemoteDir
        ServiceName    = $cfg.ServiceName
        ConfigFileName = $configName
        CertFileNames  = $certNames
    }

    if ($inv.ConsoleProcessIds -and $inv.ConsoleProcessIds.Count -gt 0) {
        throw "bento-ads.exe is running on $($cfg.Host) outside the service (pid(s): $($inv.ConsoleProcessIds -join ', ')) -- it would fight the service for the MQTT client_id. Stop it first."
    }

    $xFull = $inv.VersionOutput
    if (-not $inv.ExeExists) {
        $xDisplay = 'not installed'
    } elseif ($xFull -and $xFull.StartsWith('bento-ads ')) {
        $xDisplay = $xFull
    } else {
        $prefix = if ($inv.ExeSha256) { $inv.ExeSha256.Substring(0, 12).ToLower() } else { 'unknown' }
        $xDisplay = "legacy ($prefix)"
    }
    $xTag = Get-VersionTag $xFull
    Write-Host "  remote: $xDisplay"
    Stop-Step

    Start-Step 'Compare'
    $localFiles = @{ $configName = $cfg.ConfigFile; '.env' = $cfg.EnvFile }
    for ($i = 0; $i -lt $certNames.Count; $i++) { $localFiles[$certNames[$i]] = $cfg.CertFiles[$i] }

    $changed = @()
    foreach ($name in $localFiles.Keys) {
        $localHash = (Get-FileHash -Path $localFiles[$name] -Algorithm SHA256).Hash
        $remoteHash = $inv.FileHashes.$name
        if (-not $remoteHash) {
            Write-Host "  $name`: new"
            $changed += $name
        } elseif ($remoteHash -ne $localHash) {
            Write-Host "  $name`: changed"
            $changed += $name
        } else {
            Write-Host "  $name`: unchanged"
        }
    }
    $versionChanged = ($xFull -ne $yFull)
    if ($versionChanged) {
        Write-Host "  bento-ads.exe: remote is $xDisplay, we are pushing $yFull"
        $changed += 'bento-ads.exe'
    } else {
        Write-Host "  bento-ads.exe: unchanged ($yFull)"
    }
    Stop-Step

    if (-not $versionChanged -and $changed.Count -eq 0 -and -not $Force) {
        Write-Host "Nothing to do."
        return
    }

    if (-not $Yes) {
        $resp = Read-Host "remote is $xDisplay, we are pushing $yFull, continue? [y/N]"
        if ($resp -notmatch '^(y|yes)$') {
            Write-Host "Aborted."
            return
        }
    }

    Start-Step 'Stage'
    Invoke-RemoteBlock -Block $script:PrepareDirsBlock -TargetCfg $cfg -BlockParams @{ RemoteDir = $cfg.RemoteDir } | Out-Null

    $toStage = @{}
    $filesToSend = @()
    if ($versionChanged -or $changed -contains 'bento-ads.exe') { $filesToSend += 'bento-ads.exe' }
    $filesToSend += @($changed | Where-Object { $_ -ne 'bento-ads.exe' })

    foreach ($name in $filesToSend) {
        $localPath = if ($name -eq 'bento-ads.exe') { $localExe } else { $localFiles[$name] }
        $sizeMb = [Math]::Round((Get-Item $localPath).Length / 1MB, 2)
        Write-Host "  -> $name ($sizeMb MB)"
        Send-StagedFile -TargetCfg $cfg -LocalPath $localPath -RemoteName $name
        $toStage[$name] = (Get-FileHash -Path $localPath -Algorithm SHA256).Hash
    }

    $staged = Invoke-RemoteBlock -Block $script:VerifyStagedBlock -TargetCfg $cfg -BlockParams @{
        RemoteDir      = $cfg.RemoteDir
        ExpectedHashes = $toStage
    }
    if (-not $staged.Ok) {
        throw "staged file hashes didn't match (missing: $($staged.Missing -join ', '); mismatched: $($staged.Mismatches -join ', '))"
    }
    Write-Host "  staged hashes verified ($($toStage.Keys.Count) files)"
    Stop-Step

    Start-Step 'Apply'
    $expectedPathName = "$($cfg.RemoteDir)\bento-ads.exe -c $($cfg.RemoteDir)\$configName -e $($cfg.RemoteDir)\.env"
    $needsReinstall = (-not $inv.ServiceExists) -or (-not (Test-PathNameMatches -Actual $inv.ServicePathName -Expected $expectedPathName))

    try {
        $apply = Invoke-RemoteBlock -Block $script:ApplyBlock -TargetCfg $cfg -BlockParams @{
            RemoteDir       = $cfg.RemoteDir
            ServiceName     = $cfg.ServiceName
            ConfigFileName  = $configName
            CertFileNames   = $certNames
            StagedFileNames = @($toStage.Keys)
            NeedsReinstall  = $needsReinstall
            ServiceRecovery = $cfg.ServiceRecovery
            StopTimeoutSec  = $script:StopTimeoutSec
            KeepBackups     = $script:KeepBackups
        }
    } catch {
        # ApplyBlock itself already tried to restore and restart on
        # failure (see its own catch) -- surface that outcome plainly
        # rather than letting it scroll off inside ssh's error text.
        Write-Host "Apply failed: $($_.Exception.Message)"
        throw
    }
    foreach ($line in @($apply.Steps)) { Write-Host "  $line" }
    Stop-Step

    Start-Step 'Verify'
    $verify = Invoke-RemoteBlock -Block $script:VerifyBlock -TargetCfg $cfg -BlockParams @{
        ServiceName     = $cfg.ServiceName
        SinceUtc        = $apply.ApplyTimeUtc
        ExpectedVersion = $yTag
        TimeoutSec      = $script:VerifyTimeoutSec
    }
    Write-VerifyResult $verify
    Stop-Step

    $ok = $verify.ServiceRunning -and $verify.FoundStartEvent -and (@($verify.ErrorEventsFound).Count -eq 0)
    if ($ok) {
        Write-Host "OK: $($cfg.Host) is running $yFull"
        return
    }

    Write-Host "Verify failed: running=$($verify.ServiceRunning) startEvent=$($verify.FoundStartEvent) errors=$(@($verify.ErrorEventsFound).Count)"
    $rollback = $Yes
    if (-not $Yes) {
        $resp = Read-Host "Roll back to the previous build? [y/N]"
        $rollback = ($resp -match '^(y|yes)$')
    }
    if (-not $rollback) {
        throw "Deploy verification failed; left as-is. Backup at $($apply.BackupDir)."
    }

    $rollbackResult = Invoke-RemoteBlock -Block $script:RollbackBlock -TargetCfg $cfg -BlockParams @{
        RemoteDir      = $cfg.RemoteDir
        ServiceName    = $cfg.ServiceName
        BackupDir      = $apply.BackupDir
        StopTimeoutSec = $script:StopTimeoutSec
    }
    foreach ($line in @($rollbackResult.Steps)) { Write-Host "  $line" }

    $reverify = Invoke-RemoteBlock -Block $script:VerifyBlock -TargetCfg $cfg -BlockParams @{
        ServiceName     = $cfg.ServiceName
        SinceUtc        = (Get-Date).ToUniversalTime().ToString('o')
        ExpectedVersion = $xTag
        TimeoutSec      = $script:VerifyTimeoutSec
    }
    Write-VerifyResult $reverify
    if ($reverify.ServiceRunning) {
        throw "Deploy verification failed; rolled back to the previous build, which is running again."
    }
    throw "Deploy verification failed, and the rollback didn't come back up either. Check $($cfg.Host) by hand."
}

#endregion

# Dot-sourcing (deploy/test-remote-blocks.ps1 does this) loads the
# functions/scriptblocks above without running Main -- InvocationName is
# "." only for a dot-sourced invocation, never a direct one.
if ($MyInvocation.InvocationName -ne '.') {
    Main
}
