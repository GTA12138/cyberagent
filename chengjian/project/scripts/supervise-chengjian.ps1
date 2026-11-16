param([string]$Addr, [string]$ProxyAddr, [string]$RuntimeDir = '')
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lifecycle.ps1')
$lock = New-ChengjianLock 'supervisor' 0
$child = $null; $identity = $null; $state = $null
try {
    Add-Type -Path (Join-Path $PSScriptRoot 'runtime-native.cs')
    Set-Location -LiteralPath $script:ChengjianRoot
    $env:CHENGJIAN_CONFIG = Join-Path $script:ChengjianRoot 'config.json'
    Remove-Item Env:CHENGJIAN_PG_DSN -ErrorAction SilentlyContinue
    $env:CHENGJIAN_SKILL_DIR = Join-Path $script:ChengjianRoot 'skills'
    if ($RuntimeDir) {
        $node = Get-ChildItem -LiteralPath $RuntimeDir -Directory -Filter 'node-*-win-x64' | Sort-Object Name -Descending | Select-Object -First 1
        if ($node) { $env:PATH = $node.FullName + ';' + $env:PATH }
    }
    $state = [ordered]@{ version = 1; project = $script:ChengjianRoot; status = 'starting'; addr = $Addr; proxy = $ProxyAddr; supervisor = (Get-ChengjianIdentity (Get-Process -Id $PID)); app = $null; stdout = ''; stderr = ''; last_exit = $null }
    Write-ChengjianState $state
    $delay = 1
    while (-not (Test-Path -LiteralPath $script:ChengjianStopFile)) {
        $stamp = Get-Date -Format 'yyyyMMdd-HHmmss-fff'
        $state.stdout = Join-Path $script:ChengjianRoot "logs/chengjian-$stamp.stdout.log"
        $state.stderr = Join-Path $script:ChengjianRoot "logs/chengjian-$stamp.stderr.log"
        $arguments = '-addr ' + $Addr + ' -proxy ' + $ProxyAddr + ' -data "' + (Join-Path $script:ChengjianRoot 'data') + '"'
        $child = [ChengjianNative]::Launch($script:ChengjianBinary, $arguments, $script:ChengjianRoot, $state.stdout, $state.stderr)
        $identity = Get-ChengjianIdentity $child
        $state.app = $identity; $state.status = 'running'
        Write-ChengjianState $state
        $clock = [Diagnostics.Stopwatch]::StartNew()
        while (-not $child.WaitForExit(250)) {
            if (Test-Path -LiteralPath $script:ChengjianStopFile) {
                $state.status = 'stopping'; Write-ChengjianState $state
                Send-ChengjianStop $identity
                if (-not $child.WaitForExit(10000)) {
                    Write-Warning 'Graceful shutdown timed out; terminating the owned process tree.'
                    Stop-ChengjianTree $identity
                }
                break
            }
        }
        $child.WaitForExit()
        $code = $child.ExitCode
        $state.last_exit = $code; $state.app = $null
        $child.Dispose(); $child = $null; $identity = $null
        if ((Test-Path -LiteralPath $script:ChengjianStopFile) -or $code -eq 0) { break }
        $state.status = 'restarting'; Write-ChengjianState $state
        if ($code -eq 75) { $delay = 1; continue }
        if ($clock.Elapsed.TotalSeconds -gt 60) { $delay = 1 }
        Write-Warning "CHENGJIAN exited with code $code. Retrying in $delay seconds."
        for ($i = 0; $i -lt $delay * 4; $i++) {
            if (Test-Path -LiteralPath $script:ChengjianStopFile) { break }
            Start-Sleep -Milliseconds 250
        }
        $delay = [Math]::Min(30, $delay * 2)
    }
} catch {
    Write-Error -ErrorAction Continue $_
    if ($state) { $state.status = 'failed' }
} finally {
    if ($identity) { Stop-ChengjianTree $identity }
    if ($state) {
        if ($state.status -ne 'failed') { $state.status = 'stopped' }
        $state.app = $null; $state.supervisor = $null; Write-ChengjianState $state
    }
    $lock.ReleaseMutex(); $lock.Dispose()
}
