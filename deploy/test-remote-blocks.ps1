#Requires -Version 7.0
<#
.SYNOPSIS
    Exercises deploy.ps1's remote scriptblocks against a temp directory on
    THIS machine, without ssh -- deploy.ps1 ships them to a real target
    as-is, so this is what stands in for testing them against machine-01
    without touching it.
.DESCRIPTION
    Needs an elevated session: several scenarios install, start, stop and
    uninstall a real "bento-ads" Windows service. Refuses to run at all if
    one already exists, so it never touches a real install.

    Scenarios: fresh install, same-version no-op detection, upgrade with a
    changed config, console-process detection, the verify step, the
    rollback path, and cleanup. Prints PASS/FAIL per scenario and writes a
    timestamped report next to this script.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

# Dot-sourced: loads the blocks/functions below without running Main (see
# the InvocationName check at the bottom of deploy.ps1).
. (Join-Path $PSScriptRoot 'deploy.ps1')

$ServiceName = 'bento-ads'
$reportPath = Join-Path $PSScriptRoot ("test-remote-blocks-report-{0}.txt" -f (Get-Date -Format 'yyyyMMdd-HHmmss'))
$reportLines = [System.Collections.Generic.List[string]]::new()
$results = [System.Collections.Generic.List[object]]::new()

function Write-ReportLine {
    param([string]$Line)
    Write-Host $Line
    $reportLines.Add($Line)
}

function Invoke-Scenario {
    param([Parameter(Mandatory)][string]$Name, [Parameter(Mandatory)][scriptblock]$Body)
    Write-ReportLine ""
    Write-ReportLine "== $Name =="
    try {
        & $Body
        Write-ReportLine "PASS: $Name"
        $results.Add([PSCustomObject]@{ Name = $Name; Result = 'PASS' })
    } catch {
        Write-ReportLine "FAIL: $Name -- $($_.Exception.Message)"
        $results.Add([PSCustomObject]@{ Name = $Name; Result = 'FAIL' })
    }
}

function Assert {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw "assertion failed: $Message" }
}

# -- safety checks --------------------------------------------------------------

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "This harness installs/removes a real Windows service; run it from an elevated PowerShell."
}
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    throw "A '$ServiceName' service already exists on this machine; refusing to run (these scenarios stop/uninstall it)."
}

# -- fixtures ---------------------------------------------------------------------

$TempRoot = Join-Path $env:TEMP ("bento-ads-deploy-test-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $TempRoot -Force | Out-Null
$remoteDir = Join-Path $TempRoot 'remote'
Write-ReportLine "RemoteDir (temp): $remoteDir"

$exePath = Join-Path $TempRoot 'build\bento-ads.exe'
New-Item -ItemType Directory -Path (Split-Path $exePath) -Force | Out-Null
Push-Location $RepoRoot
try {
    go build -trimpath -o $exePath ./cmd/bento-ads
    if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
}
$builtVersion = (& $exePath --version).Trim()
$builtTag = Get-VersionTag $builtVersion
Write-ReportLine "Built: $builtVersion"

# generate->drop, http disabled: runs standalone with no PLC/port
# dependencies, so it's safe both as the installed service and as the
# stray console process in scenario 4.
$configNameV1 = 'config.yaml'
$configContentV1 = @'
input:
  generate:
    mapping: 'root = {}'
    interval: 1s
output:
  drop: {}
http:
  enabled: false
'@
$configContentV2 = @'
input:
  generate:
    mapping: 'root = {"n": timestamp_unix()}'
    interval: 1s
output:
  drop: {}
http:
  enabled: false
'@

$localConfigPath = Join-Path $TempRoot 'source\config.yaml'
New-Item -ItemType Directory -Path (Split-Path $localConfigPath) -Force | Out-Null
Set-Content -Path $localConfigPath -Value $configContentV1 -NoNewline

$localEnvPath = Join-Path $TempRoot 'source\.env'
Set-Content -Path $localEnvPath -Value "# test harness .env`n" -NoNewline

$recovery = @{ RestartDelaySec1 = 60; RestartDelaySec2 = 60; RestartDelaySec3 = 300; ResetDays = 1 }

try {
    Invoke-Scenario 'fresh install' {
        & $script:PrepareDirsBlock @{ RemoteDir = $remoteDir } | Out-Null
        Assert (Test-Path $remoteDir) "RemoteDir created"
        Assert (Test-Path (Join-Path $remoteDir 'staging')) "staging dir created"

        Copy-Item $exePath (Join-Path $remoteDir 'staging\bento-ads.exe')
        Copy-Item $localConfigPath (Join-Path $remoteDir "staging\$configNameV1")
        Copy-Item $localEnvPath (Join-Path $remoteDir 'staging\.env')

        $expectedHashes = @{
            'bento-ads.exe' = (Get-FileHash $exePath -Algorithm SHA256).Hash
            $configNameV1   = (Get-FileHash $localConfigPath -Algorithm SHA256).Hash
            '.env'          = (Get-FileHash $localEnvPath -Algorithm SHA256).Hash
        }
        $staged = & $script:VerifyStagedBlock @{ RemoteDir = $remoteDir; ExpectedHashes = $expectedHashes }
        Assert $staged.Ok "staged hashes match what was sent"

        $invBefore = & $script:InventoryBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName; ConfigFileName = $configNameV1; CertFileNames = @() }
        Assert (-not $invBefore.ServiceExists) "no service before the first apply"

        $script:expectedPathName = "$remoteDir\bento-ads.exe -c $remoteDir\$configNameV1 -e $remoteDir\.env"
        $script:apply1 = & $script:ApplyBlock @{
            RemoteDir       = $remoteDir
            ServiceName     = $ServiceName
            ConfigFileName  = $configNameV1
            CertFileNames   = @()
            StagedFileNames = @('bento-ads.exe', $configNameV1, '.env')
            NeedsReinstall  = $true
            ServiceRecovery = $recovery
            StopTimeoutSec  = 45
            KeepBackups     = 5
        }
        Assert $script:apply1.Reinstalled "service installed"

        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        Assert ($svc -and $svc.Status -eq 'Running') "service running after fresh install"

        $invAfter = & $script:InventoryBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName; ConfigFileName = $configNameV1; CertFileNames = @() }
        Assert (Test-PathNameMatches -Actual $invAfter.ServicePathName -Expected $script:expectedPathName) "installed PathName matches what we expected to install"
    }

    Invoke-Scenario 'same-version no-op detection' {
        $inv = & $script:InventoryBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName; ConfigFileName = $configNameV1; CertFileNames = @() }
        Assert ($inv.VersionOutput -eq $builtVersion) "remote version already matches what we'd push"
        Assert ($inv.FileHashes.$configNameV1 -eq (Get-FileHash $localConfigPath -Algorithm SHA256).Hash) "config hash already matches"
        Assert ($inv.FileHashes.'.env' -eq (Get-FileHash $localEnvPath -Algorithm SHA256).Hash) "env hash already matches"
        # This is exactly what Main's Compare step uses to print "Nothing to do."
    }

    Invoke-Scenario 'upgrade with changed config' {
        Set-Content -Path $localConfigPath -Value $configContentV2 -NoNewline
        $newHash = (Get-FileHash $localConfigPath -Algorithm SHA256).Hash

        $invBefore = & $script:InventoryBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName; ConfigFileName = $configNameV1; CertFileNames = @() }
        Assert ($invBefore.FileHashes.$configNameV1 -ne $newHash) "changed config hash differs from what's deployed"

        Copy-Item $localConfigPath (Join-Path $remoteDir "staging\$configNameV1") -Force
        $staged = & $script:VerifyStagedBlock @{ RemoteDir = $remoteDir; ExpectedHashes = @{ $configNameV1 = $newHash } }
        Assert $staged.Ok "staged config hash matches"

        $script:apply2 = & $script:ApplyBlock @{
            RemoteDir       = $remoteDir
            ServiceName     = $ServiceName
            ConfigFileName  = $configNameV1
            CertFileNames   = @()
            StagedFileNames = @($configNameV1)
            NeedsReinstall  = $false
            ServiceRecovery = $recovery
            StopTimeoutSec  = 45
            KeepBackups     = 5
        }
        Assert (-not $script:apply2.Reinstalled) "upgrade did not reinstall the service"

        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        Assert ($svc -and $svc.Status -eq 'Running') "service running again after the upgrade"

        $invAfter = & $script:InventoryBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName; ConfigFileName = $configNameV1; CertFileNames = @() }
        Assert ($invAfter.FileHashes.$configNameV1 -eq $newHash) "remote config now matches what we pushed"
    }

    Invoke-Scenario 'console-process detection' {
        $consoleConfig = Join-Path $TempRoot 'source\console-config.yaml'
        Set-Content -Path $consoleConfig -Value $configContentV1 -NoNewline
        $proc = Start-Process -FilePath $exePath -ArgumentList @('-c', $consoleConfig) -PassThru -WindowStyle Hidden
        try {
            Start-Sleep -Seconds 2
            $inv = & $script:InventoryBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName; ConfigFileName = $configNameV1; CertFileNames = @() }
            Assert (@($inv.ConsoleProcessIds) -contains $proc.Id) "console run detected as a non-service bento-ads.exe"
        } finally {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
        Start-Sleep -Seconds 1
        $invAfter = & $script:InventoryBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName; ConfigFileName = $configNameV1; CertFileNames = @() }
        Assert (-not (@($invAfter.ConsoleProcessIds) -contains $proc.Id)) "console pid gone after stopping it"
    }

    Invoke-Scenario 'verify step' {
        $verify = & $script:VerifyBlock @{
            ServiceName     = $ServiceName
            SinceUtc        = $script:apply2.ApplyTimeUtc
            ExpectedVersion = $builtTag
            TimeoutSec      = 20
        }
        Assert $verify.ServiceRunning "verify sees the service running"
        Assert $verify.FoundStartEvent "verify found the matching 'starting' Event Log entry"
        Assert ((@($verify.ErrorEventsFound)).Count -eq 0) "no ERROR events logged since start"
    }

    Invoke-Scenario 'rollback path' {
        $backupConfigPath = Join-Path $script:apply2.BackupDir $configNameV1
        $backupConfigHash = (Get-FileHash $backupConfigPath -Algorithm SHA256).Hash
        $liveConfigHash = (Get-FileHash (Join-Path $remoteDir $configNameV1) -Algorithm SHA256).Hash
        Assert ($backupConfigHash -ne $liveConfigHash) "backup holds the pre-upgrade config, not what's live now"

        & $script:RollbackBlock @{
            RemoteDir      = $remoteDir
            ServiceName    = $ServiceName
            BackupDir      = $script:apply2.BackupDir
            StopTimeoutSec = 45
        } | Out-Null

        $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        Assert ($svc -and $svc.Status -eq 'Running') "service running again after rollback"

        $restoredHash = (Get-FileHash (Join-Path $remoteDir $configNameV1) -Algorithm SHA256).Hash
        Assert ($restoredHash -eq $backupConfigHash) "rolled-back config matches the backup"
    }
} finally {
    Invoke-Scenario 'cleanup' {
        & $script:UninstallBlock @{ RemoteDir = $remoteDir; ServiceName = $ServiceName } | Out-Null
        Assert (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) "service uninstalled"
        Remove-Item -Path $TempRoot -Recurse -Force -ErrorAction SilentlyContinue
        Assert (-not (Test-Path $TempRoot)) "temp dir removed"
    }
}

Write-ReportLine ""
$failed = @($results | Where-Object { $_.Result -eq 'FAIL' })
Write-ReportLine ("{0}/{1} scenarios passed" -f ($results.Count - $failed.Count), $results.Count)
Set-Content -Path $reportPath -Value $reportLines
Write-Host "Report written to $reportPath"

if ($failed.Count -gt 0) { exit 1 }
