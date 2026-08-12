@echo off
setlocal
rem Lykuro Private Gateway installer (Windows amd64).
rem Downloads the release zip from GitHub Releases, verifies it against
rem checksums.txt, and places private-gateway.exe under
rem %LOCALAPPDATA%\Lykuro\bin. Nothing else: no service registration,
rem no autostart (how to daemonize is left to the operator).
rem
rem NOTE: keep this file ASCII-only. cmd.exe parses batch files in the
rem system codepage (e.g. CP932), and UTF-8 multibyte text corrupts parsing.
rem
rem Usage:  install.bat [vX.Y.Z]     (default: latest release)

set "REPO=lykuroai/Native-LLM-Platform"
set "INSTALL_DIR=%LOCALAPPDATA%\Lykuro\bin"
set "VERSION=%~1"

if "%VERSION%"=="" (
  for /f "usebackq delims=" %%v in (`powershell -NoProfile -Command "(Invoke-RestMethod 'https://api.github.com/repos/%REPO%/releases/latest').tag_name"`) do set "VERSION=%%v"
)
if "%VERSION%"=="" (
  echo failed to resolve latest release
  exit /b 1
)

set "ASSET=private-gateway_%VERSION%_windows_amd64.zip"
set "BASE=https://github.com/%REPO%/releases/download/%VERSION%"
set "TMPDIR=%TEMP%\lykuro-install-%RANDOM%"
mkdir "%TMPDIR%" || exit /b 1

echo ==^> downloading %ASSET% (%VERSION%)
curl -fsSL -o "%TMPDIR%\%ASSET%" "%BASE%/%ASSET%" || goto :fail
curl -fsSL -o "%TMPDIR%\checksums.txt" "%BASE%/checksums.txt" || goto :fail

echo ==^> verifying checksum
set "WANT="
for /f "tokens=1,2" %%a in ('findstr /c:"%ASSET%" "%TMPDIR%\checksums.txt"') do set "WANT=%%a"
if "%WANT%"=="" (
  echo checksums.txt has no entry for %ASSET%
  goto :fail
)
set "GOT="
for /f "skip=1 tokens=1" %%h in ('certutil -hashfile "%TMPDIR%\%ASSET%" SHA256') do if not defined GOT set "GOT=%%h"
if /i not "%GOT%"=="%WANT%" (
  echo checksum mismatch: %GOT% != %WANT%
  goto :fail
)

echo ==^> installing to %INSTALL_DIR%
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"
tar -xf "%TMPDIR%\%ASSET%" -C "%TMPDIR%" || goto :fail
move /y "%TMPDIR%\private-gateway_%VERSION%_windows_amd64.exe" "%INSTALL_DIR%\private-gateway.exe" >nul || goto :fail

rem Add to the user PATH unless already present.
echo %PATH% | findstr /i /c:"%INSTALL_DIR%" >nul || (
  powershell -NoProfile -Command "$p=[Environment]::GetEnvironmentVariable('Path','User'); if(-not (($p -split ';') -contains '%INSTALL_DIR%')){[Environment]::SetEnvironmentVariable('Path', ($p.TrimEnd(';') + ';%INSTALL_DIR%'), 'User')}"
  echo added %INSTALL_DIR% to your user PATH ^(open a new terminal to pick it up^)
)

"%INSTALL_DIR%\private-gateway.exe" version
echo.
echo Get started:
echo   private-gateway init     ^(detect runtimes and generate gateway.yaml^)
echo   private-gateway serve
rmdir /s /q "%TMPDIR%"
exit /b 0

:fail
echo install failed
rmdir /s /q "%TMPDIR%" 2>nul
exit /b 1
