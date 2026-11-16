param(
    [string]$Addr = '127.0.0.1:8787',
    [string]$ProxyAddr = '127.0.0.1:8788',
    [switch]$OpenBrowser,
    [switch]$Check
)
$ErrorActionPreference = 'Stop'
& (Join-Path $PSScriptRoot 'project/start.ps1') @PSBoundParameters
