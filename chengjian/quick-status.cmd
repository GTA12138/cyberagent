@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0project\status.ps1" %*
set "result=%errorlevel%"
if not "%result%"=="0" (
  echo CHENGJIAN status failed. See the error above.
  if not defined CHENGJIAN_NO_PAUSE pause
)
pause
exit /b %result%
