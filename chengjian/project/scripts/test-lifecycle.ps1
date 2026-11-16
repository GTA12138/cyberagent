# Integration checks against a disposable database and a copied launcher.
# Does not load the project's config.json or touch its live data directory.
param([switch]$UseExistingBinary)
$ErrorActionPreference = 'Stop'
$project = Split-Path $PSScriptRoot -Parent
$workspace = Split-Path (Split-Path $project -Parent) -Parent
$pgBin = Join-Path $workspace '.local/pgsql/bin'
$run = Join-Path $project ('.build/lifecycle-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
$fixture = Join-Path $run 'project with spaces'
$pgData = Join-Path $run 'postgres'
New-Item -ItemType Directory -Force -Path $fixture,(Join-Path $fixture 'scripts') | Out-Null
function Free-Port {
    $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
    $l.Start(); $port = $l.LocalEndpoint.Port; $l.Stop(); return $port
}
$dbPort = Free-Port; $httpPort = Free-Port; $proxyPort = Free-Port
$address = '127.0.0.1:' + $httpPort; $proxyAddress = '127.0.0.1:' + $proxyPort
$results = [Collections.Generic.List[string]]::new()
$started = $false; $sentinel = $null; $occupied = $null
$oldConfig = $env:CHENGJIAN_CONFIG; $oldDsn = $env:CHENGJIAN_PG_DSN
function Assert-That([bool]$Condition, [string]$Name) {
    if (-not $Condition) { throw "FAIL: $Name" }
    $results.Add($Name); Write-Host "PASS: $Name"
}
try {
    foreach ($file in @('start.ps1','stop.ps1','status.ps1')) { Copy-Item -LiteralPath (Join-Path $project $file) -Destination $fixture }
    foreach ($file in @('lifecycle.ps1','supervise-chengjian.ps1','send-stop.ps1','runtime-native.cs')) { Copy-Item -LiteralPath (Join-Path $PSScriptRoot $file) -Destination (Join-Path $fixture 'scripts') }
    $env:CHENGJIAN_CONFIG = Join-Path $run 'absent.json'; $env:CHENGJIAN_PG_DSN = ''
    if ($UseExistingBinary) { Copy-Item -LiteralPath (Join-Path $project 'chengjian.exe') -Destination $fixture }
    else {
        Push-Location $project
        try { & go build -tags embedui -o (Join-Path $fixture 'chengjian.exe') ./cmd/chengjian; if ($LASTEXITCODE -ne 0) { throw 'Build failed.' } } finally { Pop-Location }
    }
    & (Join-Path $pgBin 'initdb.exe') -D $pgData -U chengjian_test --auth-local=trust --auth-host=trust --no-locale -E UTF8 *> (Join-Path $run 'initdb.log')
    if ($LASTEXITCODE -ne 0) { throw 'Isolated initdb failed.' }
    & (Join-Path $pgBin 'pg_ctl.exe') -D $pgData -l (Join-Path $run 'postgres.log') -o "-h 127.0.0.1 -p $dbPort" -w -t 20 start
    if ($LASTEXITCODE -ne 0) { throw 'Isolated PostgreSQL failed.' }
    $started = $true
    & (Join-Path $pgBin 'createdb.exe') -h 127.0.0.1 -p $dbPort -U chengjian_test chengjian_lifecycle
    if ($LASTEXITCODE -ne 0) { throw 'Create isolated database failed.' }
    $configJson = @{database=@{host='127.0.0.1';port=$dbPort;user='chengjian_test';dbname='chengjian_lifecycle';sslmode='disable'}} | ConvertTo-Json
    [IO.File]::WriteAllText((Join-Path $fixture 'config.json'),$configJson,[Text.UTF8Encoding]::new($false))
    $launch = Join-Path $fixture 'start.ps1'; $stop = Join-Path $fixture 'stop.ps1'
    $statePath = Join-Path $fixture 'data/runtime/lifecycle.json'
    & $launch -Addr $address -ProxyAddr $proxyAddress -Check
    Assert-That (-not (Test-Path -LiteralPath $statePath)) 'Check performs no launch'
    & $launch -Addr $address -ProxyAddr $proxyAddress -TimeoutSeconds 30
    $state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
    $first = $state.app.pid
    Assert-That ([bool](Get-Process -Id $first -ErrorAction SilentlyContinue)) 'Start application from a path containing spaces'
    & $launch -Addr $address -ProxyAddr $proxyAddress
    $state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
    Assert-That ($state.app.pid -eq $first) 'Repeated start keeps one process'
    $refused = $false
    try { & $launch -Addr ('127.0.0.1:' + (Free-Port)) -ProxyAddr $proxyAddress } catch { $refused = $true }
    Assert-That $refused 'Port changes require an explicit stop'
    Stop-Process -Id $first -Force
    $timer = [Diagnostics.Stopwatch]::StartNew()
    do {
        Start-Sleep -Milliseconds 300
        $state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
    } while ((-not $state.app -or $state.app.pid -eq $first) -and $timer.Elapsed.TotalSeconds -lt 15)
    Assert-That ([bool]($state.app -and $state.app.pid -ne $first)) 'Supervisor restarts a crashed app'
    & $launch -Addr $address -ProxyAddr $proxyAddress
    $beforeStop = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
    & $stop
    $stopped = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
    Assert-That ($stopped.last_exit -eq 0) 'Graceful stop returns application exit code zero'
    Assert-That ([bool](Select-String -LiteralPath $beforeStop.stderr -Pattern 'shutting down' -Quiet)) 'Application shutdown handler actually ran'
    Assert-That (-not (Get-Process -Id $beforeStop.app.pid -ErrorAction SilentlyContinue)) 'Stop removes app process'
    & $stop
    Assert-That $true 'Repeated stop succeeds'
    & (Join-Path $pgBin 'pg_ctl.exe') -D $pgData status *> $null
    Assert-That ($LASTEXITCODE -eq 0) 'Stop preserves PostgreSQL'
    $occupied = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,$httpPort); $occupied.Start()
    $refused = $false
    try { & $launch -Addr $address -ProxyAddr $proxyAddress } catch { $refused = $true }
    Assert-That $refused 'Occupied port is rejected without killing its owner'
    $occupied.Stop(); $occupied = $null
    # A saved PID pointing at this test host must never authorize killing it.
    $stopped.supervisor = @{pid=$PID;started='1';path=(Get-Process -Id $PID).Path}
    $stopped | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $statePath -Encoding UTF8
    & $stop
    Assert-That $true 'Stale/reused PID is ignored'
    & $launch -Addr $address -ProxyAddr $proxyAddress
    & $stop -Force
    Assert-That (-not (Get-Process -Name chengjian -ErrorAction SilentlyContinue | Where-Object {$_.Path -eq (Join-Path $fixture 'chengjian.exe')})) 'Force stop leaves no owned app'
    $badConfig = @{database=@{host='127.0.0.1';port=$dbPort;user='missing_lifecycle_role';dbname='chengjian_lifecycle';sslmode='disable'}} | ConvertTo-Json
    [IO.File]::WriteAllText((Join-Path $fixture 'config.json'),$badConfig,[Text.UTF8Encoding]::new($false))
    $refused = $false
    try { & $launch -Addr $address -ProxyAddr $proxyAddress -TimeoutSeconds 5 } catch { $refused = $true }
    Assert-That $refused 'Startup failure is reported'
    $failed = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
    Assert-That (-not $failed.app -and -not $failed.supervisor) 'Startup failure cleans up supervisor and app'
} finally {
    if ($occupied) { $occupied.Stop() }
    try { if (Test-Path -LiteralPath (Join-Path $fixture 'stop.ps1')) { & (Join-Path $fixture 'stop.ps1') -Force } }
    finally { if ($started) { & (Join-Path $pgBin 'pg_ctl.exe') -D $pgData -m fast -w -t 20 stop } }
    $env:CHENGJIAN_CONFIG = $oldConfig; $env:CHENGJIAN_PG_DSN = $oldDsn
    @{passed=$results.ToArray();output=$run} | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $run 'summary.json') -Encoding UTF8
    Write-Host "Lifecycle test output: $run"
}
