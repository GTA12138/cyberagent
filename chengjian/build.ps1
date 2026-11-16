param(
    [switch]$SkipFrontend,
    [switch]$InstallDependencies,
    [string]$Version = ('chengjian-paper-' + (Get-Date -Format 'yyyyMMdd-HHmm')),
    [string]$Proxy = ''
)
$ErrorActionPreference = 'Stop'
& (Join-Path $PSScriptRoot 'project/build.ps1') @PSBoundParameters
