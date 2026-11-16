# start-lab.ps1 — 靶场一键启停（Windows 本机，127.0.0.x 多回环模拟多主机）。
#
#   powershell -ExecutionPolicy Bypass -File start-lab.ps1           启动
#   powershell -ExecutionPolicy Bypass -File start-lab.ps1 -Stop     停止
#   powershell -ExecutionPolicy Bypass -File start-lab.ps1 -Status   看状态
#   powershell -ExecutionPolicy Bypass -File start-lab.ps1 -Verify   全链路自检
#
# 真正的启停逻辑在 labctl.py 里：本脚本只负责挑一个带 paramiko 的解释器再转交。
# 之所以不用 Start-Process：Windows PowerShell 5.1 在环境里同时存在 http_proxy 与
# HTTP_PROXY 时会直接抛「已添加项。字典中的关键字…」而启动不了任何进程——带代理
# 工具的开发机经常是这个状态，靶场不该被这种事卡住。
param(
    [switch]$Stop,
    [switch]$Status,
    [switch]$Verify
)
$ErrorActionPreference = 'Stop'
$lab = $PSScriptRoot
$ctl = Join-Path $lab 'labctl.py'
if (-not (Test-Path -LiteralPath $ctl)) { throw "missing $ctl" }

# 挑解释器：必须是带 paramiko 的那个（内网 SSH 靶标与自检都依赖它）
$py = $null
foreach ($cand in @('python', 'python3', 'py')) {
    $exe = Get-Command $cand -ErrorAction SilentlyContinue
    if (-not $exe) { continue }
    $resolved = & $exe.Source -c "import sys,paramiko;print(sys.executable)" 2>$null
    if ($LASTEXITCODE -eq 0 -and $resolved) { $py = $resolved.Trim(); break }
}
if (-not $py) {
    throw '需要带 paramiko 的 Python：pip install paramiko（或把这样的 python 放到 PATH 前面）'
}

$action = 'start'
if ($Stop) { $action = 'stop' }
elseif ($Status) { $action = 'status' }
elseif ($Verify) { $action = 'verify' }

& $py $ctl $action
exit $LASTEXITCODE
