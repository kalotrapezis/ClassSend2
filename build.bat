@echo off
setlocal

rem Single source of truth for the version. Bump the trailing letter on each
rem rebuild so the user can tell which build the EXEs/installer came from.
rem Must match setup\classsend2.iss MyAppVersion.
set VERSION=0.0.3

rem ISO-8601 build timestamp via PowerShell (wmic is deprecated on modern Windows).
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-ddTHH:mm:ss'"`) do set BUILDTIME=%%I

set VFLAGS=-X classsend/internal/buildinfo.Version=%VERSION% -X classsend/internal/buildinfo.BuildTime=%BUILDTIME%

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

echo Building monitoring.exe...
go build -ldflags="-H windowsgui %VFLAGS%" -o monitoring.exe ./cmd/monitoring
if errorlevel 1 goto :err

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
"%ISCC%" setup\classsend2.iss
if errorlevel 1 goto :err

echo   dist\ClassSend2-Setup-v%VERSION%.exe  -- installer for teacher or student PC
goto :eof

:err
echo.
echo Build FAILED.
exit /b 1
