param(
    [string]$PostgresBin = '',
    [string]$GoExe = '',
    [string]$NodeExe = '',
    [string[]]$Packages = @('./...'),
    [string]$Run = '',
    [string]$SummaryPath = '',
    [switch]$SkipFrontend
)
$ErrorActionPreference = 'Stop'
$projectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$workspaceRoot = Split-Path (Split-Path $projectRoot -Parent) -Parent
$runRoot = Join-Path $projectRoot ('.build/verification/' + (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [Guid]::NewGuid().ToString('N').Substring(0, 6))
$pgData = Join-Path $runRoot 'postgres'
if (-not $PostgresBin) { $PostgresBin = Join-Path $workspaceRoot '.local/pgsql/bin' }
if (-not $GoExe) {
    # A bare `go` depends on the caller's PATH, which differs between an IDE
    # terminal, a CI shell and the EnvDock shim. Fall back to the shim before
    # giving up, otherwise this script fails for a reason unrelated to the code.
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) {
        $taskGoRoot = (& $cmd.Source env GOROOT).Trim()
        if ($LASTEXITCODE -eq 0 -and $taskGoRoot) { $GoExe = Join-Path $taskGoRoot 'bin/go.exe' }
    }
    if (-not $GoExe -or -not (Test-Path -LiteralPath $GoExe -PathType Leaf)) {
        $shim = Join-Path $env:LOCALAPPDATA 'EnvDock/shims/go.cmd'
        if (Test-Path -LiteralPath $shim -PathType Leaf) {
            $taskGoRoot = (& $shim env GOROOT).Trim()
            if ($taskGoRoot) { $GoExe = Join-Path $taskGoRoot 'bin/go.exe' }
        }
    }
    if (-not $GoExe) { throw 'Go is unavailable. Pass -GoExe.' }
}
if (-not $NodeExe) { $NodeExe = Join-Path $workspaceRoot '.local/node-v24.21.0-win-x64/node.exe' }
foreach ($required in @($GoExe, (Join-Path $PostgresBin 'initdb.exe'), (Join-Path $PostgresBin 'pg_ctl.exe'))) {
    if (-not (Test-Path -LiteralPath $required -PathType Leaf)) { throw "Missing test runtime: $required" }
}
if (-not $SkipFrontend -and -not (Test-Path -LiteralPath $NodeExe -PathType Leaf)) { throw 'Node.js is unavailable. Pass -NodeExe or -SkipFrontend.' }

# Start-Process forwards the whole environment block, and on Windows the proxy
# keys are case-insensitive: a shell that exports both `http_proxy` and
# `HTTP_PROXY` makes Start-Process throw ArgumentException ("an item with the
# same key has already been added"). The tests do not need a developer proxy, so
# drop the duplicates here and restore them in the finally block. Removing only
# the upper-case copies is not enough - which spelling survives is arbitrary.
$proxyNames = @('http_proxy', 'https_proxy', 'all_proxy', 'no_proxy')
$savedProxy = @{}
foreach ($n in $proxyNames) {
    $savedProxy[$n] = [Environment]::GetEnvironmentVariable($n)
    foreach ($variant in @($n, $n.ToUpperInvariant())) {
        if (Test-Path "Env:$variant") { Remove-Item "Env:$variant" -ErrorAction SilentlyContinue }
    }
}

$oldConfig = $env:CHENGJIAN_CONFIG
$oldDsn = $env:CHENGJIAN_PG_DSN
$oldLiveReview = $env:CHENGJIAN_REVIEW_LIVE_CONFIG
$initialized = $false
$summary = [ordered]@{ status = 'running'; go_tests_passed = 0; go_tests_skipped = 0; frontend = 'not_run'; output = $runRoot }
New-Item -ItemType Directory -Path $runRoot -Force | Out-Null
Push-Location $projectRoot
try {
    & (Join-Path $PostgresBin 'initdb.exe') -D $pgData -U chengjian_test --auth-local=trust --auth-host=trust --no-locale -E UTF8 *> (Join-Path $runRoot 'initdb.log')
    if ($LASTEXITCODE -ne 0) { throw 'Isolated initdb failed; see initdb.log.' }
    $initialized = $true
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    $testPort = $listener.LocalEndpoint.Port
    $listener.Stop()
    # Direct invocation, not Start-Process: its .ExitCode is unreliable on
    # PowerShell 5.1 and reported a failure for a server that had in fact
    # started. The -o value needs no extra quoting here - PowerShell quotes a
    # single argument that contains spaces by itself, whereas Start-Process
    # required the nested quotes.
    #
    # No redirection of any kind on this line. pg_ctl hands its stdout handle to
    # the postgres daemon it spawns, so both `| Out-Null` and `*> file` make
    # PowerShell wait for a handle the daemon holds for its whole lifetime: the
    # script then hangs right here, and the hang looks exactly like "the app
    # never started". pgtest.ps1 and verify-start.ps1 call it the same way.
    & (Join-Path $PostgresBin 'pg_ctl.exe') -D $pgData -l (Join-Path $runRoot 'postgres.log') -o "-h 127.0.0.1 -p $testPort" -w -t 20 start
    if ($LASTEXITCODE -ne 0) {
        if (Test-Path (Join-Path $runRoot 'postgres.log')) { Get-Content (Join-Path $runRoot 'postgres.log') -Tail 20 }
        throw 'Isolated PostgreSQL startup failed; see postgres.log.'
    }
    $env:CHENGJIAN_CONFIG = Join-Path $runRoot 'no-runtime-config.json'
    $env:CHENGJIAN_REVIEW_LIVE_CONFIG = $null
    $env:CHENGJIAN_PG_DSN = 'postgres://chengjian_test@127.0.0.1:' + $testPort + '/chengjian_test?sslmode=disable'
    & $GoExe vet @Packages *> (Join-Path $runRoot 'go-vet.log')
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed; see go-vet.log.' }
    $testArgs = @('test', '-json', '-p', '1', '-count=1', '-timeout', '180s')
    if ($Run) { $testArgs += @('-run', $Run) }
    & $GoExe @testArgs @Packages *> (Join-Path $runRoot 'go-test.jsonl')
    $testExit = $LASTEXITCODE
    $events = @(Get-Content -LiteralPath (Join-Path $runRoot 'go-test.jsonl') | ForEach-Object { $_ | ConvertFrom-Json })
    $summary.go_tests_passed = @($events | Where-Object { $_.Action -eq 'pass' -and $_.Test }).Count
    $summary.go_tests_skipped = @($events | Where-Object { $_.Action -eq 'skip' -and $_.Test }).Count
    if ($testExit -ne 0) {
        $events | Where-Object { $_.Action -eq 'fail' -or $_.Output -match '--- FAIL:|panic:' } | ForEach-Object { $_ | ConvertTo-Json -Compress }
        throw 'Go regression failed; see go-test.jsonl.'
    }
    if (-not $SkipFrontend) {
        Push-Location (Join-Path $projectRoot 'web')
        try {
            & $NodeExe node_modules/typescript/bin/tsc --noEmit *> (Join-Path $runRoot 'typecheck.log')
            if ($LASTEXITCODE -ne 0) { throw 'TypeScript check failed; see typecheck.log.' }
            $testFiles = @(rg --files src -g '*.test.mjs')
            & $NodeExe --test @testFiles *> (Join-Path $runRoot 'frontend-test.log')
            if ($LASTEXITCODE -ne 0) { throw 'Frontend tests failed; see frontend-test.log.' }
            $summary.frontend = 'passed'
        } finally { Pop-Location }
    }
    $summary.status = 'passed'
} catch {
    $summary.status = 'failed'
    throw
} finally {
    try {
        if ($initialized) {
            # Direct invocation instead of Start-Process, for two reasons:
            #   1. Start-Process forwards the whole environment block, which
            #      throws on duplicate proxy keys (see the removal above).
            #   2. Its .ExitCode is unreliable on PowerShell 5.1 - it reported a
            #      failure on a shutdown that pg_ctl had actually completed.
            # pgtest.ps1 uses the direct form for the same two reasons.
            & (Join-Path $PostgresBin 'pg_ctl.exe') -D $pgData -m immediate -w -t 20 stop
            if ($LASTEXITCODE -ne 0) {
                # Do NOT throw from here. A throw inside `finally` replaces the
                # exception that got us here, so a broken shutdown would hide the
                # real test failure - which is exactly what happened once and
                # cost a debugging round. Record it and let the original surface.
                $summary.status = 'failed'
                $summary['shutdown_error'] = 'pg_ctl stop exit=' + $LASTEXITCODE
                Write-Warning ('Isolated PostgreSQL shutdown failed (exit=' + $LASTEXITCODE + ').')
            }
        }
    } catch {
        $summary.status = 'failed'
        $summary['shutdown_error'] = 'shutdown threw: ' + $_.Exception.Message
        Write-Warning ('Isolated PostgreSQL shutdown threw: ' + $_.Exception.Message)
    } finally {
        $env:CHENGJIAN_CONFIG = $oldConfig
        $env:CHENGJIAN_PG_DSN = $oldDsn
        $env:CHENGJIAN_REVIEW_LIVE_CONFIG = $oldLiveReview
        foreach ($n in $savedProxy.Keys) {
            if ($null -ne $savedProxy[$n]) { [Environment]::SetEnvironmentVariable($n, $savedProxy[$n]) }
        }
        Pop-Location
        $summary | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $runRoot 'summary.json') -Encoding UTF8
        if ($SummaryPath) { $summary | ConvertTo-Json | Set-Content -LiteralPath $SummaryPath -Encoding UTF8 }
        $summary | ConvertTo-Json
    }
}
