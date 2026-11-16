param(
    [switch]$SkipFrontend,
    [switch]$InstallDependencies,
    [string]$Version = ('chengjian-paper-' + (Get-Date -Format 'yyyyMMdd-HHmm')),
    [string]$Proxy = ''
)
$ErrorActionPreference = 'Stop'
$project = $PSScriptRoot
$workspace = Split-Path (Split-Path $project -Parent) -Parent
$runtime = Join-Path $workspace '.local'
$nodeDir = Join-Path $runtime 'node-v24.21.0-win-x64'
if ($Version -notmatch '^[a-zA-Z0-9._-]+$') { throw 'Version may contain letters, digits, dots, underscores and hyphens only.' }
$active = Get-CimInstance Win32_Process -Filter "Name='chengjian.exe'" | Where-Object { $_.ExecutablePath -eq (Join-Path $project 'chengjian.exe') }
if ($active) { throw 'CHENGJIAN is running. Run stop.ps1 before replacing its executable.' }
if (Test-Path -LiteralPath (Join-Path $nodeDir 'node.exe')) { $env:Path = "$nodeDir;$env:Path" }
$goRoot = (& go env GOROOT).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Go is unavailable.' }
$goExe = Join-Path $goRoot 'bin/go.exe'
$node = (Get-Command node -ErrorAction Stop).Source
if ($Proxy) { $env:HTTP_PROXY = $Proxy; $env:HTTPS_PROXY = $Proxy }
$env:CGO_ENABLED = '0'
$env:NEXT_EXPORT = '1'
$env:NEXT_TELEMETRY_DISABLED = '1'
$env:HUSKY = '0'
Push-Location $project
try {
    if (-not $SkipFrontend) {
        Push-Location (Join-Path $project 'web')
        try {
            if ($InstallDependencies -or -not (Test-Path 'node_modules/next')) {
                & npm.cmd ci --no-audit --no-fund
                if ($LASTEXITCODE -ne 0) { throw 'npm ci failed.' }
            }
            & $node node_modules/next/dist/bin/next build
            if ($LASTEXITCODE -ne 0) { throw 'Frontend build failed.' }
        } finally { Pop-Location }
    }
    if (-not (Test-Path 'web/out/index.html')) { throw 'Static UI is missing. Run build.ps1 without -SkipFrontend.' }
    $embedded = [IO.Path]::GetFullPath((Join-Path $project 'server/webui/dist'))
    $expected = [IO.Path]::GetFullPath($project) + [IO.Path]::DirectorySeparatorChar
    if (-not $embedded.StartsWith($expected, [StringComparison]::OrdinalIgnoreCase)) { throw 'Invalid generated UI directory.' }
    if (Test-Path -LiteralPath $embedded) {
        if ((Get-Item -LiteralPath $embedded).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Generated UI directory must not be a link.' }
        Remove-Item -LiteralPath $embedded -Recurse -Force
    }
    New-Item -ItemType Directory -Force $embedded | Out-Null
    Copy-Item -Path 'web/out/*' -Destination $embedded -Recurse -Force
    New-Item -ItemType Directory -Force '.build' | Out-Null
    & $goExe build -tags embedui -trimpath "-ldflags=-X main.version=$Version" -o '.build/chengjian.exe' ./cmd/chengjian
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
    Copy-Item -LiteralPath '.build/chengjian.exe' -Destination 'chengjian.exe' -Force
    $hash = (Get-FileHash 'chengjian.exe' -Algorithm SHA256).Hash.ToLowerInvariant()
    "$hash  chengjian.exe" | Set-Content 'chengjian.exe.sha256' -Encoding ascii
    [ordered]@{ project = 'CHENGJIAN'; version = $Version; built_at = (Get-Date).ToUniversalTime().ToString('o'); sha256 = $hash; executable = (Join-Path $project 'chengjian.exe') } |
        ConvertTo-Json | Set-Content 'build-info.json' -Encoding UTF8
    Write-Host "Built CHENGJIAN: $project\chengjian.exe"
    Write-Host "Version: $Version"
} finally { Pop-Location }
