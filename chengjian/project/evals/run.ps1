param([switch]$RequireAll)
$ErrorActionPreference = 'Stop'
# -Encoding UTF8 is mandatory on both reads below. PowerShell 5.1 decodes a
# BOM-less file with the ANSI code page; the manifest and the summary are UTF-8
# with Chinese text, so the byte stream gets cut mid-sequence, JSON quoting
# breaks, and ConvertFrom-Json fails with "invalid array" pointing at a line
# that looks perfectly fine.
$manifest = Get-Content -LiteralPath (Join-Path $PSScriptRoot 'scenarios.json') -Raw -Encoding UTF8 | ConvertFrom-Json
$ready = @($manifest.scenarios | Where-Object { $_.status -eq 'automated' })
$gaps = @($manifest.scenarios | Where-Object { $_.status -ne 'automated' })
foreach ($gap in $gaps) {
    # Manual entries carry 'gap' (what is still missing); older entries used 'reason'.
    $why = if ($gap.gap) { $gap.gap } else { $gap.reason }
    Write-Host ("GAP {0}: {1} -- {2}" -f $gap.id, $gap.name, $why)
}
$packages = @($ready.package | Sort-Object -Unique)
$testNames = @($ready | ForEach-Object { $_.tests } | Sort-Object -Unique)
$pattern = '^(' + (($testNames | ForEach-Object { [Regex]::Escape($_) }) -join '|') + ')$'
$resultDir = Join-Path $PSScriptRoot ('../.build/evals/' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $resultDir -Force | Out-Null
$summaryPath = Join-Path $resultDir 'summary.json'
& (Join-Path $PSScriptRoot '../scripts/test.ps1') -Packages $packages -Run $pattern -SkipFrontend -SummaryPath $summaryPath
$summary = Get-Content -LiteralPath $summaryPath -Raw -Encoding UTF8 | ConvertFrom-Json
$events = @(Get-Content -LiteralPath (Join-Path $summary.output 'go-test.jsonl') -Encoding UTF8 | ForEach-Object { $_ | ConvertFrom-Json })
foreach ($scenario in $ready) {
    $suffix = '/' + $scenario.package.TrimStart('.', '/')
    foreach ($expected in $scenario.tests) {
        $passed = @($events | Where-Object { $_.Action -eq 'pass' -and $_.Test -ceq $expected -and $_.Package.EndsWith($suffix) })
        if ($passed.Count -ne 1) { throw "Scenario $($scenario.id): expected test $expected did not pass exactly once (missing, renamed, or skipped)." }
    }
}
if ($RequireAll -and $gaps.Count -gt 0) { throw 'Implemented scenarios passed, but the manifest still has gaps.' }
Write-Host ("Automated scenarios passed: {0}; manual gaps: {1} (of {2} registered)." -f $ready.Count, $gaps.Count, @($manifest.scenarios).Count)
if ($gaps.Count -gt 0) {
    Write-Host 'Manual gaps are not failures of this run: they need a real target, a real browser or a real restart.'
}
