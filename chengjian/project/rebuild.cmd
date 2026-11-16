@echo off
chcp 65001 >nul 2>&1
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1" %*
if errorlevel 1 (
  echo CHENGJIAN build failed.
  pause
  exit /b 1
)
