# Shared Windows lifecycle functions. Configuration contents are never printed.
$script:ChengjianRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..')).TrimEnd('\')
$script:ChengjianBinary = Join-Path $script:ChengjianRoot 'chengjian.exe'
$script:ChengjianStateDir = Join-Path $script:ChengjianRoot 'data/runtime'
$script:ChengjianStateFile = Join-Path $script:ChengjianStateDir 'lifecycle.json'
$script:ChengjianStopFile = Join-Path $script:ChengjianStateDir 'stop.request'
function New-ChengjianLock([string]$Kind, [int]$TimeoutSeconds = 90) {
    $sha = [Security.Cryptography.SHA256]::Create()
    try { $hash = [BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($script:ChengjianRoot.ToLowerInvariant()))).Replace('-','').Substring(0,24) } finally { $sha.Dispose() }
    $mutex = New-Object Threading.Mutex($false, "Local\CHENGJIAN-$Kind-$hash")
    try {
        try { $owned = $mutex.WaitOne([TimeSpan]::FromSeconds($TimeoutSeconds)) } catch [Threading.AbandonedMutexException] { $owned = $true }
        if (-not $owned) { throw 'Another CHENGJIAN lifecycle operation is still running.' }
        return $mutex
    } catch { $mutex.Dispose(); throw }
}
function Read-ChengjianState {
    for ($attempt = 0; $attempt -lt 4; $attempt++) {
        if (-not (Test-Path -LiteralPath $script:ChengjianStateFile)) { return $null }
        try { $state = [IO.File]::ReadAllText($script:ChengjianStateFile) | ConvertFrom-Json; break }
        catch { if ($attempt -eq 3) { throw 'Invalid/unreadable lifecycle.json; inspect data/runtime before restarting.' }; Start-Sleep -Milliseconds 50 }
    }
    if ($state.project -ne $script:ChengjianRoot) { throw 'Lifecycle state belongs to another directory. Archive lifecycle.json before starting this copy.' }
    return $state
}
function Write-ChengjianState($State) {
    $temp = $script:ChengjianStateFile + '.' + $PID + '.tmp'
    [IO.File]::WriteAllText($temp, ($State | ConvertTo-Json -Depth 5), [Text.UTF8Encoding]::new($false))
    for ($attempt = 0; $attempt -lt 4; $attempt++) {
        try {
            if (Test-Path -LiteralPath $script:ChengjianStateFile) { [IO.File]::Replace($temp,$script:ChengjianStateFile,[NullString]::Value) }
            else { [IO.File]::Move($temp,$script:ChengjianStateFile) }
            break
        } catch { if ($attempt -eq 3) { throw }; Start-Sleep -Milliseconds 50 }
    }
}
function Get-ChengjianIdentity($Process) {
    return @{ pid = $Process.Id; started = $Process.StartTime.ToUniversalTime().Ticks.ToString(); path = $Process.Path }
}
function Find-ChengjianProcess($Identity) {
    if (-not $Identity -or -not $Identity.pid -or -not $Identity.started -or -not $Identity.path) { return $null }
    try {
        $p = Get-Process -Id ([int]$Identity.pid) -ErrorAction Stop
        if ($p.StartTime.ToUniversalTime().Ticks.ToString() -eq [string]$Identity.started -and $p.Path -eq $Identity.path) { return $p }
    } catch { }
    return $null
}
function Get-ChengjianApplications {
    return @(Get-Process -Name chengjian -ErrorAction SilentlyContinue | Where-Object { try { $_.Path -eq $script:ChengjianBinary } catch { $false } })
}
function Get-ChengjianShell { return (Join-Path $env:SystemRoot 'System32/WindowsPowerShell/v1.0/powershell.exe') }
function Test-ChengjianPort([string]$HostName, [int]$Port) {
    $client = New-Object Net.Sockets.TcpClient
    try {
        $attempt = $client.BeginConnect($HostName, $Port, $null, $null)
        if (-not $attempt.AsyncWaitHandle.WaitOne(800)) { return $false }
        $client.EndConnect($attempt)
        return $client.Connected
    } catch { return $false } finally { $client.Dispose() }
}
function Get-ChengjianHealth([string]$Address) {
    $request = [Net.HttpWebRequest]::Create("http://$Address/api/health")
    $request.Proxy = $null; $request.Timeout = 1500; $request.ReadWriteTimeout = 1500
    $response = $null; $reader = $null
    try {
        $response = $request.GetResponse()
        $reader = New-Object IO.StreamReader($response.GetResponseStream())
        return $reader.ReadToEnd() | ConvertFrom-Json
    } finally { if ($reader) { $reader.Dispose() }; if ($response) { $response.Dispose() } }
}
function Stop-ChengjianTree($Identity) {
    $p = Find-ChengjianProcess $Identity
    if ($p) {
        if ($p.Path -ne $script:ChengjianBinary) {
            $details = Get-CimInstance Win32_Process -Filter "ProcessId=$($p.Id)"
            $expected = '"' + (Join-Path $script:ChengjianRoot 'scripts/supervise-chengjian.ps1') + '"'
            if (-not $details.CommandLine -or -not $details.CommandLine.Contains($expected)) { throw 'Refusing to stop a process outside this project.' }
        }
        # Windows PowerShell 5.1 treats native stderr as terminating errors.
        # A child that exits during taskkill is harmless; verify the root after it.
        $previousPreference = $ErrorActionPreference
        try {
            $ErrorActionPreference = 'Continue'
            & "$env:SystemRoot/System32/taskkill.exe" /PID $p.Id /T /F *> $null
        } finally { $ErrorActionPreference = $previousPreference }
        if (Find-ChengjianProcess $Identity) { throw "Could not stop owned process $($p.Id)." }
    }
}
function Send-ChengjianStop($Identity) {
    if (-not (Find-ChengjianProcess $Identity)) { return }
    $helper = Join-Path $PSScriptRoot 'send-stop.ps1'
    $args = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$helper`" -ProcessId $($Identity.pid) -Started $($Identity.started)"
    $p = Start-Process -FilePath (Get-ChengjianShell) -ArgumentList $args -WindowStyle Hidden -PassThru
    if (-not $p.WaitForExit(5000)) { $p.Kill() }
}
