# pgtest.ps1 - minimal isolated PostgreSQL + go test runner.
#
# Why a separate script instead of scripts/test.ps1:
#   1. test.ps1 launches pg_ctl via Start-Process, which forwards the whole
#      environment block. On Windows with both http_proxy and HTTP_PROXY set,
#      Start-Process throws (duplicate key, case-insensitive) and the script
#      dies for reasons unrelated to the code under test.
#   2. Full verification also builds the frontend and runs quality gates; this
#      script only needs initdb -> test -> stop -> cleanup.
#
# Usage:
#   ./scripts/pgtest.ps1                           # everything
#   ./scripts/pgtest.ps1 -Packages ./db ./server   # selected packages
#   ./scripts/pgtest.ps1 -Run TestRelease          # selected tests
#   ./scripts/pgtest.ps1 -Keep                     # keep the data dir for debugging
param(
    [string[]]$Packages = @('./...'),
    [string]$Run = '',
    [int]$Port = 0,
    [string]$PostgresBin = '',
    [switch]$Keep,
    [switch]$VerboseTest
)
# NOTE: keep 'Continue' (not 'Stop'). PowerShell 5.1 turns native stderr into
# ErrorRecords, so with 'Stop' any harmless NOTICE from initdb/createdb aborts
# the run. Every native call below checks $LASTEXITCODE explicitly instead.
$ErrorActionPreference = 'Continue'

# Resolve the Go toolchain explicitly. A bare `go` depends on the caller's PATH,
# which differs between an IDE terminal, a CI shell and the EnvDock shim; when it
# is missing the script used to die silently right after pg_ctl start (the native
# call fails, $LASTEXITCODE stays unset, and nothing is printed about why).
$goExe = ''
$cmd = Get-Command go -ErrorAction SilentlyContinue
if ($cmd) { $goExe = $cmd.Source }
if (-not $goExe) {
    $shim = Join-Path $env:LOCALAPPDATA 'EnvDock/shims/go.cmd'
    if (Test-Path -LiteralPath $shim -PathType Leaf) { $goExe = $shim }
}
if (-not $goExe) { throw 'go toolchain not found (add it to PATH or install the EnvDock shim)' }

$projectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$workspaceRoot = Split-Path (Split-Path $projectRoot -Parent) -Parent
if (-not $PostgresBin) { $PostgresBin = Join-Path $workspaceRoot '.local/pgsql/bin' }
$initdb = Join-Path $PostgresBin 'initdb.exe'
$pgctl = Join-Path $PostgresBin 'pg_ctl.exe'
$createdb = Join-Path $PostgresBin 'createdb.exe'
foreach ($bin in @($initdb, $pgctl, $createdb)) {
    if (-not (Test-Path -LiteralPath $bin -PathType Leaf)) { throw "missing PostgreSQL binary: $bin" }
}

# Fresh data dir every run: reusing one lets rows from a previous run pollute
# this run's assertions (classic symptom: a CIDR rule matches an old company
# row and looks like a code bug).
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$runRoot = Join-Path $projectRoot ('.build/pgtest/' + $stamp)
$pgData = Join-Path $runRoot 'data'
$pgLog = Join-Path $runRoot 'postgres.log'
New-Item -ItemType Directory -Force -Path $runRoot | Out-Null

function Test-PortFree([int]$p) {
    try {
        $l = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, $p)
        $l.Start()
        $l.Stop()
        return $true
    } catch {
        return $false
    }
}

if ($Port -eq 0) {
    $Port = 55432
    while (-not (Test-PortFree $Port) -and $Port -lt 55600) { $Port++ }
}

$oldDsn = $env:CHENGJIAN_PG_DSN
$oldConfig = $env:CHENGJIAN_CONFIG
$started = $false
$exitCode = 1
try {
    Write-Host "[pgtest] initdb -> $pgData"
    & $initdb -D $pgData -U chengjian_test --auth-local=trust --auth-host=trust --no-locale -E UTF8 *> (Join-Path $runRoot 'initdb.log')
    if ($LASTEXITCODE -ne 0) { throw "initdb failed, see $(Join-Path $runRoot 'initdb.log')" }

    Write-Host "[pgtest] start on 127.0.0.1:$Port"
    & $pgctl -D $pgData -l $pgLog -o "-h 127.0.0.1 -p $Port" -w -t 30 start
    if ($LASTEXITCODE -ne 0) {
        if (Test-Path $pgLog) { Get-Content $pgLog -Tail 40 }
        throw 'pg_ctl start failed'
    }
    $started = $true

    & $createdb -h 127.0.0.1 -p $Port -U chengjian_test chengjian_test 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { Write-Host "[pgtest] createdb exit=$LASTEXITCODE (continuing; schema is applied by the tests)" }
    $env:CHENGJIAN_PG_DSN = "postgres://chengjian_test@127.0.0.1:$Port/chengjian_test?sslmode=disable"
    # Tests must not pick up a real LLM config from the developer machine.
    $env:CHENGJIAN_CONFIG = ''

    Write-Host "[pgtest] go test $($Packages -join ' ')"
    $goArgs = @('test') + $Packages + @('-count=1', '-p', '1')
    if ($Run) { $goArgs += @('-run', $Run) }
    if ($VerboseTest) { $goArgs += '-v' }
    & $goExe @goArgs
    $exitCode = $LASTEXITCODE
} finally {
    if ($started) {
        & $pgctl -D $pgData -m immediate -w -t 20 stop | Out-Null
        Write-Host "[pgtest] stopped (exit=$exitCode)"
    }
    $env:CHENGJIAN_PG_DSN = $oldDsn
    $env:CHENGJIAN_CONFIG = $oldConfig
    if (-not $Keep -and (Test-Path $runRoot)) {
        Remove-Item -LiteralPath $runRoot -Recurse -Force -ErrorAction SilentlyContinue
    } elseif ($Keep) {
        Write-Host "[pgtest] kept: $runRoot"
    }
}
exit $exitCode
