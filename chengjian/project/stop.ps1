[CmdletBinding()]
param([switch]$Force)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'scripts/lifecycle.ps1')
$lock = New-ChengjianLock 'operation'
try {
    New-Item -ItemType Directory -Force -Path $script:ChengjianStateDir | Out-Null
    $state = Read-ChengjianState
    [IO.File]::WriteAllText($script:ChengjianStopFile, 'stop requested')
    $supervisor = if ($state) { Find-ChengjianProcess $state.supervisor } else { $null }
    if ($supervisor) {
        if ($Force) { Stop-ChengjianTree $state.supervisor }
        elseif (-not $supervisor.WaitForExit(18000)) {
            Write-Warning 'Supervisor did not stop in time; terminating its owned process tree.'
            Stop-ChengjianTree $state.supervisor
        }
    }
    # Stop the legacy supervisor before its app to prevent a restart race.
    $legacy = Join-Path $script:ChengjianRoot 'scripts/supervise-chengjian.ps1'
    Get-CimInstance Win32_Process | Where-Object {
        $_.Name -in @('powershell.exe','pwsh.exe') -and $_.CommandLine -and $_.CommandLine.Contains('"' + $legacy + '"')
    } | ForEach-Object { Stop-ChengjianTree (Get-ChengjianIdentity (Get-Process -Id $_.ProcessId)) }
    foreach ($app in @(Get-ChengjianApplications)) {
        $identity = Get-ChengjianIdentity $app
        if (-not $Force -and $state -and $state.app -and $state.app.started -eq $identity.started -and $state.app.pid -eq $identity.pid) {
            Send-ChengjianStop $identity
            $null = $app.WaitForExit(10000)
        }
        if (Find-ChengjianProcess $identity) {
            Write-Warning 'Stopping a remaining project process tree forcibly.'
            Stop-ChengjianTree $identity
        }
    }
    if (@(Get-ChengjianApplications).Count -gt 0) { throw 'CHENGJIAN still has a running process.' }
    $latest = Read-ChengjianState
    if ($latest) { $state = $latest }
    if ($state) { $state.status = 'stopped'; $state.app = $null; $state.supervisor = $null; Write-ChengjianState $state }
    Remove-Item -LiteralPath (Join-Path $script:ChengjianStateDir 'supervisor.pid') -Force -ErrorAction SilentlyContinue
    Write-Host 'CHENGJIAN stopped. Shared PostgreSQL remains running.'
} finally { $lock.ReleaseMutex(); $lock.Dispose() }
