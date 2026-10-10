# Build the host and agent from this checkout and install them on this machine without a UAC prompt, through the
# preauthorized on-demand task "HyperHand Dev Install" (register it once with scripts\dev-install-setup.ps1 from an
# elevated PowerShell). On success the installed files equal the build, the service runs and exactly one installed
# tray process is up. Then call vm_update_agent for each VM. Development and testing only.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev-install.ps1 [-NoBuild] [-TimeoutSeconds 180]
param([switch]$NoBuild, [int]$TimeoutSeconds = 180)
$ErrorActionPreference = 'Stop'
$taskName = 'HyperHand Dev Install'
$repo = Split-Path $PSScriptRoot -Parent
$candidateDir = Join-Path $repo 'build\dev-install'
$candidate = Join-Path $candidateDir 'hyperhand.exe'
$installedDir = Join-Path $env:ProgramFiles 'HyperHand'
$log = Join-Path $env:LOCALAPPDATA 'HyperHand\hyperhand.log'

function Fail([string]$message) {
    Write-Error $message -ErrorAction Continue
    exit 1
}

$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
try { $task = $scheduler.GetFolder('\').GetTask($taskName) } catch { $task = $null }
if ($null -eq $task) {
    Fail ("The task '$taskName' is not registered, so installing would need UAC. Register it once from an elevated " +
          "PowerShell: powershell -NoProfile -ExecutionPolicy Bypass -File `"$PSScriptRoot\dev-install-setup.ps1`"")
}
if ($task.Definition.Actions.Item(1).Path -ne $candidate) { Fail "The task '$taskName' runs $($task.Definition.Actions.Item(1).Path), not $candidate." }
if ($task.State -in @(2, 4)) { Fail 'A development install is queued or running; wait for it before building or installing again.' }

if (-not $NoBuild) {
    $null = New-Item -ItemType Directory -Path $candidateDir -Force
    $commit = (& git -C $repo rev-parse --short HEAD).Trim()
    if ((& git -C $repo status --porcelain -- cmd internal go.mod go.sum)) { $commit += '-dirty' }
    $version = 'dev-' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + $commit
    Push-Location -LiteralPath $repo
    try {
        foreach ($name in @('hyperhand', 'hyperhand-agent')) {
            & go build -ldflags ('-H windowsgui -X hyperhand/internal/proto.Version=' + $version) -o (Join-Path $candidateDir "$name.exe") "./cmd/$name"
            if ($LASTEXITCODE -ne 0) { Fail "go build failed: $name" }
        }
    } finally { Pop-Location }
    Write-Output "built $version"
}
$hashes = @{}
foreach ($name in @('hyperhand.exe', 'hyperhand-agent.exe')) {
    $path = Join-Path $candidateDir $name
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { Fail "missing $path; build first (run without -NoBuild)" }
    $hashes[$name] = (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash
}

$previousRun = $task.LastRunTime
while ((Get-Date) -lt $previousRun.AddSeconds(2)) { Start-Sleep -Milliseconds 250 } # LastRunTime has a resolution of seconds
$null = $task.Run($null)
$deadline = (Get-Date).AddSeconds($TimeoutSeconds)
do {
    Start-Sleep -Milliseconds 500
    $task = $scheduler.GetFolder('\').GetTask($taskName)
    $done = $task.LastRunTime -ne $previousRun -and $task.State -notin @(2, 4)
    if (-not $done -and (Get-Date) -ge $deadline) { Fail "the install task did not finish within $TimeoutSeconds s; it was not stopped: inspect it before retrying" }
} until ($done)
if ($task.LastTaskResult -ne 0) {
    $tail = if (Test-Path -LiteralPath $log) { (Get-Content -LiteralPath $log -Tail 3) -join "`n" } else { '' }
    Fail ("the installer failed (task result $($task.LastTaskResult)). Last log lines ($log):`n$tail`n" +
          "If the HyperHand service is now stopped, run this script again with -NoBuild: the installer restores it.")
}
foreach ($name in @('hyperhand.exe', 'hyperhand-agent.exe')) {
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $installedDir $name)).Hash -ne $hashes[$name]) { Fail "the installed $name is not the build" }
}
if ((Get-Service HyperHandService).Status -ne 'Running') { Fail 'the HyperHand service is not running after the install' }
$trayDeadline = (Get-Date).AddSeconds(15)
do {
    $tray = @(Get-Process hyperhand -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq (Join-Path $installedDir 'hyperhand.exe') })
    if ($tray.Count -eq 1) { break }
    Start-Sleep -Milliseconds 250
} while ((Get-Date) -lt $trayDeadline)
if ($tray.Count -ne 1) { Fail "expected one installed tray process, found $($tray.Count)" }
Write-Output "installed without UAC: service running, tray pid $($tray[0].Id). Next: vm_update_agent for each VM."
