$runtime = Join-Path (Split-Path $PSScriptRoot -Parent) '.local\node-v24.21.0-win-x64'
& "$runtime\node.exe" "$runtime\node_modules\@playwright\cli\playwright-cli.js" @args
exit $LASTEXITCODE
