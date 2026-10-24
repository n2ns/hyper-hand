param([string]$CompilerPath)

# Exercise the real Inno script with a harmless payload; never run the product installer.
$ErrorActionPreference = 'Stop'
if (-not $CompilerPath) {
    $CompilerPath = @(
        "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
        "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe"
    ) | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
}
if (-not $CompilerPath -or -not (Test-Path -LiteralPath $CompilerPath)) {
    throw 'Inno Setup 6 ISCC.exe was not found; supply -CompilerPath.'
}

function Get-InstallationSnapshot {
    $keys = @(
        'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall',
        'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall',
        'HKCU:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall'
    )
    $entries = foreach ($key in $keys) {
        if (Test-Path -LiteralPath $key) {
            Get-ChildItem -LiteralPath $key | ForEach-Object {
                $entry = Get-ItemProperty -LiteralPath $_.PSPath
                if ($entry.DisplayName -like '*HyperHand*' -or $_.PSChildName -like '*HyperHand*') {
                    [ordered]@{
                        Key = $_.Name
                        DisplayName = $entry.DisplayName
                        DisplayVersion = $entry.DisplayVersion
                        UninstallString = $entry.UninstallString
                        QuietUninstallString = $entry.QuietUninstallString
                    }
                }
            }
        }
    }
    $files = foreach ($name in @('hyperhand.exe', 'hyperhand-agent.exe')) {
        $file = Join-Path ([Environment]::GetFolderPath('ProgramFiles')) "HyperHand\$name"
        if (Test-Path -LiteralPath $file) {
            [ordered]@{ Path = $file; SHA256 = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash }
        }
    }
    [ordered]@{ Entries = @($entries); Files = @($files) } | ConvertTo-Json -Depth 5 -Compress
}

function Stop-TestProcessTree([System.Diagnostics.Process]$Process) {
    # Only descendants of the process launched by this test are eligible for cleanup.
    if ($Process.HasExited) { return }
    $all = @(Get-CimInstance Win32_Process)
    $owned = @($Process.Id)
    for ($i = 0; $i -lt $owned.Count; $i++) {
        $owned += @($all | Where-Object { $_.ParentProcessId -eq $owned[$i] } |
            Select-Object -ExpandProperty ProcessId)
    }
    [array]::Reverse($owned)
    foreach ($processId in $owned) {
        $candidate = Get-Process -Id $processId -ErrorAction SilentlyContinue
        if ($candidate -and $candidate.StartTime -ge $Process.StartTime) {
            $candidate.Kill()
            [void]$candidate.WaitForExit(5000)
        }
    }
}

$before = Get-InstallationSnapshot
$testDir = Join-Path ([IO.Path]::GetTempPath()) ('hyperhand-setup-tests-' + [guid]::NewGuid().ToString('N'))
$savedResult = $env:HYPERHAND_SETUP_TEST_RESULT
$savedExit = $env:HYPERHAND_SETUP_TEST_EXIT
$process = $null
try {
    [void](New-Item -ItemType Directory -Path $testDir)
    $payloadDir = Join-Path $testDir 'payload'
    [void](New-Item -ItemType Directory -Path $payloadDir)
    $source = Join-Path $testDir 'payload.cs'
    @'
using System;
using System.IO;
using System.Threading;
class Payload {
    static int Main(string[] args) {
        Thread.Sleep(1200);
        File.WriteAllLines(Environment.GetEnvironmentVariable("HYPERHAND_SETUP_TEST_RESULT"), args);
        return int.Parse(Environment.GetEnvironmentVariable("HYPERHAND_SETUP_TEST_EXIT"));
    }
}
'@ | Set-Content -LiteralPath $source -Encoding UTF8
    # .NET Framework produces a standalone Windows executable under either PowerShell host.
    $csc = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
    & $csc /nologo /target:winexe "/out:$payloadDir\hyperhand.exe" $source
    if ($LASTEXITCODE -ne 0) { throw "Fixture compilation failed: $LASTEXITCODE" }
    $script = Join-Path (Split-Path $PSScriptRoot -Parent) 'installer\hyperhand.iss'
    & $CompilerPath /Q /DAppVersion=0.0.0 "/DPayloadDir=$payloadDir" "/O$testDir" $script
    if ($LASTEXITCODE -ne 0) { throw "Inno Setup compilation failed: $LASTEXITCODE" }
    $setup = Join-Path $testDir 'hyperhand-0.0.0-windows-amd64-setup.exe'

    foreach ($backendExit in @(0, 23)) {
        $result = Join-Path $testDir "result-$backendExit.txt"
        $log = Join-Path $testDir "setup-$backendExit.log"
        $env:HYPERHAND_SETUP_TEST_RESULT = $result
        $env:HYPERHAND_SETUP_TEST_EXIT = [string]$backendExit
        $watch = [Diagnostics.Stopwatch]::StartNew()
        $process = Start-Process -FilePath $setup -ArgumentList @(
            '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', "/LOG=`"$log`""
        ) -PassThru
        if (-not $process.WaitForExit(45000)) {
            throw "Silent setup hung for backend exit $backendExit; a dialog may be waiting."
        }
        $process.WaitForExit()
        $watch.Stop()
        if (-not (Test-Path -LiteralPath $result)) { throw 'Setup returned before the payload completed.' }
        $arguments = [IO.File]::ReadAllLines($result)
        if ($arguments.Count -ne 2 -or $arguments[0] -ne 'install' -or $arguments[1] -ne '--wizard') {
            throw "Unexpected payload arguments: $($arguments -join ' ')"
        }
        if ($watch.ElapsedMilliseconds -lt 1200) { throw 'Setup did not wait for the delayed payload.' }
        if (($backendExit -eq 0 -and $process.ExitCode -ne 0) -or
            ($backendExit -ne 0 -and $process.ExitCode -eq 0)) {
            Get-Content -LiteralPath $log | Write-Host
            throw "Backend exit $backendExit produced unexpected setup exit $($process.ExitCode)."
        }
        Write-Host "PASS backend exit $backendExit -> setup exit $($process.ExitCode); waited for install --wizard"
        $process.Dispose()
        $process = $null
    }
    if ((Get-InstallationSnapshot) -ne $before) {
        throw 'The setup wrapper changed HyperHand binaries or uninstall registration despite using a fixture.'
    }
    Write-Host 'PASS installed binaries and HyperHand uninstall entries unchanged'
} finally {
    if ($process) {
        Stop-TestProcessTree $process
        $process.Dispose()
    }
    $env:HYPERHAND_SETUP_TEST_RESULT = $savedResult
    $env:HYPERHAND_SETUP_TEST_EXIT = $savedExit
    if (Test-Path -LiteralPath $testDir) { Remove-Item -LiteralPath $testDir -Recurse -Force }
}
