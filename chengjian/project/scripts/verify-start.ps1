# verify-start.ps1 - compile-and-start verification for one build.
#
# One PowerShell session owns the whole lifecycle (initdb -> start -> app ->
# health probe -> stop). Splitting it across invocations does not work: pg_ctl
# launches postgres as a child of the calling session, and when that session is
# reaped the server goes with it. The symptom is a later "connection refused"
# that looks exactly like an application bug.
#
# Every step prints a line before it runs. A silent exit here would be
# indistinguishable from "the app refused to start", which is the one thing this
# script exists to distinguish.
param(
    [string]$Addr = '127.0.0.1:18787',
    [int]$Port = 55434
)
$ErrorActionPreference = 'Continue'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$pgBin = 'E:\cyberagent\.local\pgsql\bin'
if (-not (Test-Path (Join-Path $pgBin 'initdb.exe'))) {
    $pgBin = Join-Path (Split-Path (Split-Path $root -Parent) -Parent) '.local/pgsql/bin'
}
if (-not (Test-Path (Join-Path $pgBin 'initdb.exe'))) {
    Write-Host '[verify] FAILED: PostgreSQL binaries not found'
    exit 2
}

# Duplicate proxy keys (http_proxy + HTTP_PROXY) make Start-Process throw on
# Windows; a loopback health check does not need them.
foreach ($n in @('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY')) {
    if (Test-Path "Env:$n") { Remove-Item "Env:$n" -ErrorAction SilentlyContinue }
}

# Fresh directory per run: reusing one after a killed postgres leaves a
# populated data dir and initdb refuses to initialise into it.
$bin = Join-Path $root ('.build/verify-' + (Get-Date -Format 'HHmmss'))
$data = Join-Path $bin 'data'
New-Item -ItemType Directory -Force -Path $bin | Out-Null
Write-Host "[verify] work dir: $bin"

$app = $null
$pgStarted = $false
$code = 1
try {
    Write-Host '[verify] initdb'
    & (Join-Path $pgBin 'initdb.exe') -D $data -U chengjian_test --auth-local=trust --auth-host=trust --no-locale -E UTF8 *> (Join-Path $bin 'initdb.log')
    if ($LASTEXITCODE -ne 0) { throw "initdb exit=$LASTEXITCODE (see $bin\initdb.log)" }

    Write-Host "[verify] start postgres on 127.0.0.1:$Port"
    # No pipe here. pg_ctl hands its stdout handle to the postgres daemon it
    # spawns, so `... start | Out-Null` never completes: PowerShell waits for
    # every writer of the pipe to close, and the daemon holds one open for its
    # whole lifetime. The symptom is the script printing this line and then
    # hanging forever, which reads exactly like "the app never started".
    & (Join-Path $pgBin 'pg_ctl.exe') -D $data -l (Join-Path $bin 'pg.log') -o "-h 127.0.0.1 -p $Port" -w -t 30 start
    if ($LASTEXITCODE -ne 0) {
        if (Test-Path (Join-Path $bin 'pg.log')) { Get-Content (Join-Path $bin 'pg.log') -Tail 20 }
        throw "pg_ctl start exit=$LASTEXITCODE"
    }
    $pgStarted = $true

    & (Join-Path $pgBin 'createdb.exe') -h 127.0.0.1 -p $Port -U chengjian_test chengjian_verify 2>&1 | Out-Null
    Write-Host "[verify] createdb exit=$LASTEXITCODE (schema is applied by the app)"

    $env:CHENGJIAN_PG_DSN = "postgres://chengjian_test@127.0.0.1:$Port/chengjian_verify?sslmode=disable"
    $env:CHENGJIAN_CONFIG = ''

    $exe = Join-Path $root '.build/chengjian-verify.exe'
    if (-not (Test-Path $exe)) { throw "missing $exe (run go build -o .build/chengjian-verify.exe ./cmd/chengjian)" }
    Write-Host "[verify] launch $exe on $Addr"
    $app = Start-Process -FilePath $exe `
        -ArgumentList @('-addr', $Addr, '-data', (Join-Path $bin 'appdata')) `
        -RedirectStandardOutput (Join-Path $bin 'app-out.log') `
        -RedirectStandardError (Join-Path $bin 'app-err.log') -PassThru
    Write-Host "[verify] pid=$($app.Id)"

    $health = $null
    $deadline = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $deadline) {
        if ($app.HasExited) { break }
        try {
            $health = Invoke-RestMethod -Uri "http://$Addr/api/health" -TimeoutSec 3
            break
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }

    if ($null -eq $health) {
        Write-Host '[verify] FAILED: /api/health never answered'
        $code = 1
    } else {
        Write-Host ("[verify] health: ok={0} service={1} version={2}" -f $health.ok, $health.service, $health.version)
        $code = 0

        # Probe 2: fail-closed auth. A protected route must refuse an anonymous
        # caller. If this ever returns 200 the deployment is wide open, and no
        # amount of "health ok" makes that acceptable.
        $anonStatus = 0
        try {
            Invoke-RestMethod -Uri "http://$Addr/api/tasks" -TimeoutSec 5 | Out-Null
            $anonStatus = 200
        } catch {
            $anonStatus = [int]$_.Exception.Response.StatusCode
        }
        if ($anonStatus -eq 401) {
            Write-Host '[verify] auth: anonymous /api/tasks -> 401 (fail-closed)'
        } else {
            Write-Host ("[verify] FAILED: anonymous /api/tasks -> {0}, expected 401" -f $anonStatus)
            $code = 1
        }

        # Probe 3: real session round trip. Initialise the password, take the
        # issued token and read a protected route with it. This exercises the
        # whole chain (handler -> bcrypt -> settings table -> JWT -> middleware),
        # which a health check alone never touches.
        $pw = 'verify-' + [Guid]::NewGuid().ToString('N').Substring(0, 12)
        try {
            $login = Invoke-RestMethod -Uri "http://$Addr/api/auth/init" -Method Post `
                -ContentType 'application/json' -Body (@{ password = $pw } | ConvertTo-Json) -TimeoutSec 10
            if (-not $login.token) { throw 'init returned no token' }
            $hdr = @{ Authorization = "Bearer $($login.token)" }
            $tasks = Invoke-RestMethod -Uri "http://$Addr/api/tasks" -Headers $hdr -TimeoutSec 10
            # The payload is {"tasks": [...], "active": "..."} - one object, not
            # an array. `@($tasks).Count` would report 1 no matter how many tasks
            # exist, which is the kind of probe that always passes.
            $n = @($tasks.tasks).Count
            Write-Host ("[verify] auth: password initialised, /api/tasks -> {0} task(s)" -f $n)
            if ($n -ne 0) {
                Write-Host ("[verify] WARN: fresh database reported {0} task(s); expected 0" -f $n)
            }
            $stats = Invoke-RestMethod -Uri "http://$Addr/api/health" -Headers $hdr -TimeoutSec 5
            Write-Host ("[verify] auth: token accepted on /api/health (ok={0})" -f $stats.ok)
        } catch {
            Write-Host ("[verify] FAILED: authenticated round trip -> " + $_.Exception.Message)
            $code = 1
        }

        # Probe 4: the schema this build depends on must actually exist. A
        # migration that silently no-ops produces a server that starts fine and
        # then fails on the first write, which is the worst possible failure
        # shape for a background agent (it looks like a target problem).
        $psql = Join-Path $pgBin 'psql.exe'
        $sql = @'
SELECT 'observations=' || to_regclass('public.observations')::text
    || ' topology_edges=' || to_regclass('public.topology_edges')::text
    || ' prereq_col=' || (SELECT count(*) FROM information_schema.columns
                          WHERE table_name='exploration_nodes' AND column_name='prerequisites')::text
    || ' donewhen_col=' || (SELECT count(*) FROM information_schema.columns
                            WHERE table_name='exploration_nodes' AND column_name='done_when')::text
    || ' reachver_col=' || (SELECT count(*) FROM information_schema.columns
                            WHERE table_name='tunnels' AND column_name='reach_verified_ranges')::text;
'@
        $schemaOut = & $psql -h 127.0.0.1 -p $Port -U chengjian_test -d chengjian_verify -t -A -c $sql 2>&1
        Write-Host ("[verify] schema: " + ($schemaOut -join ' '))
        $ok = ($schemaOut -join ' ') -match 'prereq_col=1' -and ($schemaOut -join ' ') -match 'donewhen_col=1' `
            -and ($schemaOut -join ' ') -match 'reachver_col=1' -and ($schemaOut -join ' ') -match 'observations=observations'
        if (-not $ok) {
            Write-Host '[verify] FAILED: expected tables/columns missing after startup migration'
            $code = 1
        }

        # Probe 5: subsystems that only announce themselves in the log. They are
        # wired at construction time, so "the process started" does not imply
        # "this subsystem came up" - the log line is the only proof.
        #
        # The anchors are deliberately ASCII-only. PowerShell 5.1 decodes a
        # BOM-less script using the ANSI code page, so any Chinese literal in
        # this file would arrive mangled and the probe would report a missing
        # marker on a perfectly healthy start. Each anchor below is emitted by
        # exactly one call site and contains no Chinese.
        if (Test-Path (Join-Path $bin 'app-err.log')) {
            $log = Get-Content (Join-Path $bin 'app-err.log') -Raw -Encoding UTF8
            $markers = @(
                @{ Name = 'S3 semantic layer (enrich.SetJSAnalyzer)'; Pattern = '\[enrich\] S3' },
                @{ Name = 'planner read-only intranet tools';           Pattern = '\[tools\] planner' },
                @{ Name = 'tunnel subsystem (port pool 20000-21000)';   Pattern = '20000-21000' }
            )
            foreach ($m in $markers) {
                if ($log -match $m.Pattern) {
                    Write-Host ("[verify] subsystem up: " + $m.Name)
                } else {
                    Write-Host ("[verify] FAILED: startup marker missing: " + $m.Name)
                    $code = 1
                }
            }
        }
    }
} catch {
    Write-Host ("[verify] FAILED: " + $_.Exception.Message)
    $code = 1
} finally {
    if ($app -and -not $app.HasExited) { Stop-Process -Id $app.Id -Force -ErrorAction SilentlyContinue }
    if ($pgStarted) { & (Join-Path $pgBin 'pg_ctl.exe') -D $data -m immediate -w -t 20 stop }
    if (Test-Path (Join-Path $bin 'app-err.log')) {
        Write-Host '[verify] --- app stderr (tail) ---'
        Get-Content (Join-Path $bin 'app-err.log') -Tail 30
    }
}
exit $code
