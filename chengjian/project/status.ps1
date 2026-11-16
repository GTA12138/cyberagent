$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'scripts/lifecycle.ps1')
$state = Read-ChengjianState
if (-not $state -or -not (Find-ChengjianProcess $state.app)) {
    if (@(Get-ChengjianApplications).Count -gt 0) { Write-Host 'CHENGJIAN: running outside the launcher.' } else { Write-Host 'CHENGJIAN: stopped.' }
    return
}
Write-Host "CHENGJIAN: $($state.status), PID $($state.app.pid)"
Write-Host "URL: http://$($state.addr)/"
Write-Host "Log: $($state.stderr)"
try { $health = Get-ChengjianHealth $state.addr; Write-Host "Health: $($health.ok), version $($health.version)" } catch { Write-Warning 'Process exists, but health endpoint is unavailable.' }
