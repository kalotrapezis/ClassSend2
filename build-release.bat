@echo off
rem build-release.bat — release build with logging disabled.
rem
rem Drives the same build.bat / build-win7.bat as a dev build, but with two
rem env vars set:
rem   VERSION_OVERRIDE   — clean version string (no -a/-b suffix) for the
rem                        binaries' --about output and the installer filename.
rem   RELEASE_FLAGS      — -ldflags additions; -X devlog.disabled=1 turns the
rem                        per-session log files off in the shipped binaries.
rem
rem Usage:
rem   build-release.bat            -> v0.0.4 release
rem   build-release.bat 0.0.5      -> v0.0.5 release (override version)
rem
rem Output: dist\ClassSend2-Setup-v<VERSION>.exe — the file to publish.

setlocal enabledelayedexpansion

set "REL_VERSION=0.0.4"
if not "%~1"=="" set "REL_VERSION=%~1"

echo === RELEASE BUILD v%REL_VERSION%  (devlog: OFF) ===
echo.

set "VERSION_OVERRIDE=%REL_VERSION%"
set "RELEASE_FLAGS=-X classsend/internal/devlog.disabled=1"

echo --- Step 1/2: Win7 32-bit agent (Go 1.20) ---
call .\build-win7.bat
if errorlevel 1 goto :err

echo.
echo --- Step 2/2: Modern x64 + x86 + monitoring + castviewer + installer ---
call .\build.bat
if errorlevel 1 goto :err

echo.
echo === RELEASE BUILD v%REL_VERSION% complete ===
echo   dist\ClassSend2-Setup-v%REL_VERSION%.exe
goto :eof

:err
echo.
echo Release build FAILED.
exit /b 1
