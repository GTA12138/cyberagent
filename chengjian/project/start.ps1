[CmdletBinding()]
param(
    [ValidatePattern('^127\.0\.0\.1:\d{1,5}$')][string]$Addr = '127.0.0.1:8787',
    [ValidatePattern('^127\.0\.0\.1:\d{1,5}$')][string]$ProxyAddr = '127.0.0.1:8788',
    [switch]$OpenBrowser,
    [switch]$Check,
    [ValidateRange(5,180)][int]$TimeoutSeconds = 60,
    [string]$RuntimeDir = ''
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'scripts/lifecycle.ps1')
$port = [int]($Addr.Split(':')[-1]); $proxyPort = [int]($ProxyAddr.Split(':')[-1])
if ($port -lt 1 -or $port -gt 65535 -or $proxyPort -lt 1 -or $proxyPort -gt 65535 -or $port -eq $proxyPort) { throw 'Invalid or duplicate HTTP/proxy ports.' }
$configPath = Join-Path $script:ChengjianRoot 'config.json'
foreach ($path in @($script:ChengjianBinary, $configPath)) { if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Required file missing: $path" } }
$configBytes = [IO.File]::ReadAllBytes($configPath)
try { $cfg = Get-Content -LiteralPath $configPath -Raw -Encoding UTF8 | ConvertFrom-Json } catch { throw 'config.json is not valid JSON.' }
if (-not $cfg.database) { throw 'config.json must contain a database configuration.' }
$dbHost = $cfg.database.host; $dbPort = $cfg.database.port
if ($cfg.database.dsn) {
    try { $uri = [Uri]$cfg.database.dsn; $dbHost = $uri.Host; $dbPort = $uri.Port } catch { throw 'The database DSN must be a PostgreSQL URL.' }
}
if (-not $dbHost) { $dbHost = '127.0.0.1' }
if (-not $dbPort -or $dbPort -lt 1) { $dbPort = 5432 }
if (-not $RuntimeDir) {
    $search = $script:ChengjianRoot
    while ($search) {
        $candidate = Join-Path $search '.local'
        if (Test-Path -LiteralPath (Join-Path $candidate 'pgsql/bin/pg_ctl.exe')) { $RuntimeDir = $candidate; break }
        $search = Split-Path $search -Parent
    }
}
Write-Host "Project: $script:ChengjianRoot"
Write-Host "URL: http://$Addr/"
$hashFile = Join-Path $script:ChengjianRoot 'chengjian.exe.sha256'
if (Test-Path -LiteralPath $hashFile) {
    $expected = ((Get-Content -LiteralPath $hashFile -Raw).Trim() -split '\s+')[0]
    $sha = [Security.Cryptography.SHA256]::Create()
    $binaryStream = [IO.File]::OpenRead($script:ChengjianBinary)
    try { $actual = [BitConverter]::ToString($sha.ComputeHash($binaryStream)).Replace('-','') }
    finally { $binaryStream.Dispose(); $sha.Dispose() }
    if ($actual -ne $expected) { Write-Warning 'Binary checksum differs from chengjian.exe.sha256; rebuild to refresh release metadata.' }
}
if ($Check) {
    Write-Host ("Database reachable: " + (Test-ChengjianPort $dbHost $dbPort))
    Write-Host 'Configuration and executable checks passed. No services started.'
    return
}
$lock = New-ChengjianLock 'operation'
$launched = $null
try {
    New-Item -ItemType Directory -Force -Path $script:ChengjianStateDir,(Join-Path $script:ChengjianRoot 'logs') | Out-Null
    $state = Read-ChengjianState
    $supervisor = if ($state) { Find-ChengjianProcess $state.supervisor } else { $null }
    $apps = @(Get-ChengjianApplications)
    if ($supervisor) {
        if ($state.addr -ne $Addr -or $state.proxy -ne $ProxyAddr) { throw "CHENGJIAN is already managed at $($state.addr) / $($state.proxy). Stop it before changing ports." }
    } elseif ($apps.Count -gt 0) {
        throw 'An unmanaged instance of this project is running. Run stop.ps1, then start.ps1.'
    } else {
        $listeners = [Net.NetworkInformation.IPGlobalProperties]::GetIPGlobalProperties().GetActiveTcpListeners()
        if (@($listeners | Where-Object { $_.Port -in @($port,$proxyPort) }).Count -gt 0) { throw 'HTTP or proxy port is occupied. Choose different ports; no process was stopped.' }
        if (-not (Test-ChengjianPort $dbHost $dbPort)) {
            $pgCtl = if ($RuntimeDir) { Join-Path $RuntimeDir 'pgsql/bin/pg_ctl.exe' } else { '' }
            $pgData = if ($RuntimeDir) { Join-Path $RuntimeDir 'pgdata' } else { '' }
            if ($dbHost -notin @('127.0.0.1','localhost','::1') -or -not $pgCtl -or -not (Test-Path -LiteralPath $pgData)) { throw 'Configured database is unreachable. Start it first or pass -RuntimeDir for local PostgreSQL.' }
            & $pgCtl -D $pgData status *> $null
            if ($LASTEXITCODE -ne 0) {
                & $pgCtl -D $pgData -l (Join-Path $RuntimeDir 'postgres.log') -w -t 30 start
                if ($LASTEXITCODE -ne 0) { throw 'Local PostgreSQL startup failed; see its postgres.log.' }
            }
            if (-not (Test-ChengjianPort $dbHost $dbPort)) { throw 'PostgreSQL is running, but the configured endpoint is unreachable. Check config.json host/port.' }
        }
        Remove-Item -LiteralPath $script:ChengjianStopFile -Force -ErrorAction SilentlyContinue
        $supervisorScript = Join-Path $script:ChengjianRoot 'scripts/supervise-chengjian.ps1'
        $stamp = Get-Date -Format 'yyyyMMdd-HHmmss-fff'
        $arguments = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $supervisorScript + '" -Addr ' + $Addr + ' -ProxyAddr ' + $ProxyAddr
        if ($RuntimeDir) { $arguments += ' -RuntimeDir "' + $RuntimeDir.TrimEnd('\') + '"' }
        $process = Start-Process -FilePath (Get-ChengjianShell) -ArgumentList $arguments -WorkingDirectory $script:ChengjianRoot -WindowStyle Hidden -RedirectStandardOutput (Join-Path $script:ChengjianRoot "logs/supervisor-$stamp.out.log") -RedirectStandardError (Join-Path $script:ChengjianRoot "logs/supervisor-$stamp.err.log") -PassThru
        $launched = Get-ChengjianIdentity (Get-Process -Id $process.Id)
    }
    $clock = [Diagnostics.Stopwatch]::StartNew(); $ready = $false
    while ($clock.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
        $state = Read-ChengjianState
        if ($launched -and -not (Find-ChengjianProcess $launched)) { throw 'Supervisor exited during startup. See logs/supervisor-*.err.log.' }
        if ($state -and (Find-ChengjianProcess $state.app)) {
            try {
                $health = Get-ChengjianHealth $Addr
                if ($health.ok -and $health.service -eq 'chengjian') {
                    $owners = @(Get-NetTCPConnection -State Listen -LocalPort $port,$proxyPort -ErrorAction SilentlyContinue)
                    if (@($owners | Where-Object { $_.OwningProcess -eq [int]$state.app.pid } | Select-Object -ExpandProperty LocalPort -Unique).Count -eq 2) { $ready = $true; break }
                }
            } catch { }
        }
        Start-Sleep -Milliseconds 300
    }
    if (-not $ready) { throw 'CHENGJIAN did not become ready within the timeout. See logs and data/runtime/lifecycle.json.' }
    Write-Host "CHENGJIAN ready: http://$Addr/ (PID $($state.app.pid), $($health.version))"
    if ($OpenBrowser) { Start-Process "http://$Addr/" }
} catch {
    $startupError = $_
    Write-Warning ("Startup failed: " + $_.Exception.Message)
    if ($launched) {
        try {
        [IO.File]::WriteAllText($script:ChengjianStopFile, 'startup failed')
        $p = Find-ChengjianProcess $launched
        if ($p -and -not $p.WaitForExit(16000)) { Stop-ChengjianTree $launched }
        foreach ($app in @(Get-ChengjianApplications)) { Stop-ChengjianTree (Get-ChengjianIdentity $app) }
        } catch { Write-Warning ("Startup cleanup failed: " + $_.Exception.Message) }
    }
    throw $startupError
} finally { $lock.ReleaseMutex(); $lock.Dispose() }
