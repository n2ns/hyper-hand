# One-time registration (or removal) of the on-demand task "HyperHand Dev Install", which lets scripts\dev-install.ps1
# install development builds of this checkout without a UAC prompt. Run once from an elevated PowerShell opened as the
# developer who will run dev-install.ps1 (that is the one UAC approval):
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\dev-install-setup.ps1 [-Mode Enable|Remove]
#
# The task runs <checkout>\build\dev-install\hyperhand.exe install --quiet with that user's elevated token, has no
# triggers (demand start only) and can be changed or removed only elevated. Trust: anyone who can replace that file and
# start the task runs code as an administrator. Remove it when development no longer needs it. It never replaces an
# existing task of the same name that runs something else.
param([ValidateSet('Enable', 'Remove')][string]$Mode = 'Enable')
$ErrorActionPreference = 'Stop'
$taskName = 'HyperHand Dev Install'
$candidate = Join-Path (Split-Path $PSScriptRoot -Parent) 'build\dev-install\hyperhand.exe'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error 'Run this from an elevated PowerShell (Run as administrator), signed in as the developer.' -ErrorAction Continue
    exit 1
}
$owner = $identity.User.Value
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')
try { $task = $folder.GetTask($taskName) } catch { $task = $null }
if ($null -ne $task) {
    $d = $task.Definition
    $sid = $d.Principal.UserId
    if ($sid -notlike 'S-1-*') { $sid = ([Security.Principal.NTAccount]::new($sid)).Translate([Security.Principal.SecurityIdentifier]).Value }
    if ($d.Actions.Count -ne 1 -or $d.Actions.Item(1).Path -ne $candidate -or $d.Actions.Item(1).Arguments -ne 'install --quiet' -or $sid -ne $owner) {
        Write-Error "A task named '$taskName' exists that runs something else or for another user; not touching it." -ErrorAction Continue
        exit 1
    }
    if ($task.State -in @(2, 4)) { Write-Error 'A development install is queued or running; wait for it.' -ErrorAction Continue; exit 1 }
}
if ($Mode -eq 'Remove') {
    if ($null -ne $task) { $folder.DeleteTask($taskName, 0) }
    Write-Output "removed '$taskName' (HyperHand itself stays installed)"
    exit 0
}
if ($null -ne $task) { Write-Output "'$taskName' is already registered for this checkout and user"; exit 0 }

$d = $scheduler.NewTask(0)
$d.RegistrationInfo.Description = "HyperHand development install for $candidate (scripts\dev-install.ps1). Runs that file elevated on demand."
$d.Principal.UserId = $owner
$d.Principal.LogonType = 3          # interactive token: the developer must be signed in
$d.Principal.RunLevel = 1           # highest available
$d.Settings.Enabled = $true
$d.Settings.Hidden = $true
$d.Settings.AllowDemandStart = $true
$d.Settings.DisallowStartIfOnBatteries = $false
$d.Settings.StopIfGoingOnBatteries = $false
$d.Settings.ExecutionTimeLimit = 'PT0S' # never kill an installer while it replaces files
$d.Settings.MultipleInstances = 2       # ignore a second start while one runs
$action = $d.Actions.Create(0)
$action.Path = $candidate
$action.Arguments = 'install --quiet'
$action.WorkingDirectory = Split-Path -Parent $candidate
# SYSTEM and Administrators full control; the developer may read and run it, not change it.
$sddl = 'O:BAG:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGX;;;' + $owner + ')'
$null = $folder.RegisterTaskDefinition($taskName, $d, 2, $owner, $null, 3, $sddl)
$r = $folder.GetTask($taskName).Definition
if ($r.Principal.RunLevel -ne 1 -or $r.Triggers.Count -ne 0 -or $r.Actions.Item(1).Path -ne $candidate) {
    Write-Error 'The registered task differs from what was requested; inspect it in Task Scheduler.' -ErrorAction Continue
    exit 1
}
Write-Output "registered '$taskName'; install builds with scripts\dev-install.ps1 (no UAC)"
