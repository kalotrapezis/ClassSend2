@echo off
setlocal

rem Single source of truth for the version. Bump the trailing letter on each
rem rebuild so the user can tell which build the EXEs/installer came from.
rem Must match setup\classsend2.iss MyAppVersion.
set VERSION=0.2.0

rem build-release.bat may set VERSION_OVERRIDE / RELEASE_FLAGS to ship a clean
rem versioned, logging-disabled build without editing this file.
if not "%VERSION_OVERRIDE%"=="" set VERSION=%VERSION_OVERRIDE%

rem ISO-8601 build timestamp via PowerShell (wmic is deprecated on modern Windows).
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-ddTHH:mm:ss'"`) do set BUILDTIME=%%I

set VFLAGS=-X classsend/internal/buildinfo.Version=%VERSION% -X classsend/internal/buildinfo.BuildTime=%BUILDTIME% %RELEASE_FLAGS%

echo Building teacher.exe   (v%VERSION% built %BUILDTIME%)...
go build -ldflags="-X main.defaultRole=teacher %VFLAGS%" -o teacher.exe ./cmd/classsend
if errorlevel 1 goto :err

echo Building student.exe...
go build -ldflags="-X main.defaultRole=student %VFLAGS%" -o student.exe ./cmd/classsend
if errorlevel 1 goto :err

echo Building classsend-agent.exe...
go build -ldflags="-H windowsgui %VFLAGS%" -o classsend-agent.exe ./cmd/classsend-agent
if errorlevel 1 goto :err

rem The installer pulls the modern (win10-x64) agent from dist\, not the
rem project root. Keep that copy in sync so a new build always ships.
if not exist dist mkdir dist
copy /Y classsend-agent.exe dist\classsend-agent-win10-x64.exe >nul
if errorlevel 1 goto :err

rem Win10 32-bit pair — modern Go 1.24 toolchain with GOARCH=386. Runs on
rem Win10/11 x86, but NOT on Win7 (Go 1.21+ dropped Win7 support; that's
rem what build-win7.bat is for).
echo Building Win10 x86 student.exe + agent + castviewer...
set "GOARCH=386"
go build -ldflags="-X main.defaultRole=student %VFLAGS%" -o dist\student-win10-x86.exe ./cmd/classsend
if errorlevel 1 (set "GOARCH=" & goto :err)
go build -ldflags="-H windowsgui %VFLAGS%" -o dist\classsend-agent-win10-x86.exe ./cmd/classsend-agent
if errorlevel 1 (set "GOARCH=" & goto :err)
go build -ldflags="-H windowsgui %VFLAGS%" -o dist\castviewer-win10-x86.exe ./cmd/castviewer
if errorlevel 1 (set "GOARCH=" & goto :err)
set "GOARCH="

echo Building monitoring.exe...
go build -ldflags="-H windowsgui %VFLAGS%" -o monitoring.exe ./cmd/monitoring
if errorlevel 1 goto :err

echo Building castviewer.exe...
go build -ldflags="-H windowsgui %VFLAGS%" -o castviewer.exe ./cmd/castviewer
if errorlevel 1 goto :err

rem Win7 agent must already be in dist/ (built separately by build-win7.bat
rem because Go 1.20 can't parse the modern go.mod directive). Warn loudly if
rem missing or stale relative to source — the installer needs it.
if not exist dist\classsend-agent-win7-x86.exe (
    echo.
    echo WARNING: dist\classsend-agent-win7-x86.exe missing.
    echo          Run build-win7.bat to build the Win7 32-bit agent before
    echo          shipping the installer.
)

rem Bundled ffmpeg.exe powers the optional Teacher Screen Casting installer
rem component. The binary is ~200 MB and isn't tracked in git — fetch-ffmpeg.bat
rem downloads BtbN's static GPL build into third_party\ffmpeg\. The Inno Setup
rem step below will fail loudly if it's missing, but warn earlier here so the
rem fix is obvious.
if not exist third_party\ffmpeg\ffmpeg.exe (
    echo.
    echo WARNING: third_party\ffmpeg\ffmpeg.exe missing.
    echo          Run fetch-ffmpeg.bat to download it. Without it the installer
    echo          step will fail. The bundled ffmpeg is what powers the
    echo          optional "Teacher Screen Casting" component.
)

echo.
echo Done v%VERSION%:
echo   teacher.exe          -- run on teacher PC  (needs monitoring.exe beside it)
echo   student.exe          -- chat TUI for students (needs classsend-agent.exe)
echo   classsend-agent.exe  -- hidden background agent, installed on student PCs
echo   monitoring.exe       -- place next to teacher.exe

rem Installer (requires Inno Setup). Probe both common install paths.
set "ISCC="
if exist "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" set "ISCC=C:\Program Files (x86)\Inno Setup 6\ISCC.exe"
if exist "C:\Program Files\Inno Setup 6\ISCC.exe"       set "ISCC=C:\Program Files\Inno Setup 6\ISCC.exe"

if "%ISCC%"=="" (
    echo.
    echo Skipping installer ^(Inno Setup not found^).
    echo Install from: https://jrsoftware.org/isinfo.php
    goto :eof
)

echo.
echo Building installer...
if not exist dist mkdir dist
"%ISCC%" /DMyAppVersion=%VERSION% setup\classsend2.iss
if errorlevel 1 goto :err

echo   dist\ClassSend2-Setup-v%VERSION%.exe  -- installer for teacher or student PC
goto :eof

:err
echo.
echo Build FAILED.
exit /b 1
