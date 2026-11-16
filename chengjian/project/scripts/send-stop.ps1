param([int]$ProcessId, [string]$Started)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'lifecycle.ps1')
$identity = @{ pid = $ProcessId; started = $Started; path = $script:ChengjianBinary }
if (-not (Find-ChengjianProcess $identity)) { exit 0 }
Add-Type -Path (Join-Path $PSScriptRoot 'runtime-native.cs')
[ChengjianNative]::SendBreak($ProcessId)
