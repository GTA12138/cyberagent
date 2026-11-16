@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0project\stop.ps1" %*
set "result=%errorlevel%"
if not "%result%"=="0" (
  echo CHENGJIAN stop failed. See the error above.
  if not defined CHENGJIAN_NO_PAUSE pause
)
exit /b %result%
