@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0start.ps1" -OpenBrowser %*
set "result=%errorlevel%"
if not "%result%"=="0" (
  echo CHENGJIAN start failed. See the error above.
  if not defined CHENGJIAN_NO_PAUSE pause
)
exit /b %result%
