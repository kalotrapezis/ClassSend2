@echo off
setlocal

echo Building teacher.exe...
go build -ldflags="-X main.defaultRole=teacher" -o teacher.exe ./cmd/classsend
if errorlevel 1 goto :err

echo Building student.exe...
go build -ldflags="-X main.defaultRole=student" -o student.exe ./cmd/classsend
if errorlevel 1 goto :err

echo Building classsend-agent.exe...
go build -ldflags="-H windowsgui" -o classsend-agent.exe ./cmd/classsend-agent
if errorlevel 1 goto :err

echo Building monitoring.exe...
go build -ldflags="-H windowsgui" -o monitoring.exe ./cmd/monitoring
if errorlevel 1 goto :err

echo.
echo Done:
echo   teacher.exe          -- run on teacher PC  (needs monitoring.exe beside it)
echo   student.exe          -- chat TUI for students (needs classsend-agent.exe)
echo   classsend-agent.exe  -- hidden background agent, installed on student PCs
echo   monitoring.exe       -- place next to teacher.exe

rem ── Installer (requires Inno Setup) ──────────────────────────────────────────
set ISCC=
for %%P in (
    "C:\Program Files (x86)\Inno Setup 6\ISCC.exe"
    "C:\Program Files\Inno Setup 6\ISCC.exe"
) do (
    if exist %%P set ISCC=%%P
)

if "%ISCC%"=="" (
    echo.
    echo Skipping installer ^(Inno Setup not found^).
    echo Install from: https://jrsoftware.org/isinfo.php
    goto :eof
)

echo.
echo Building installer...
if not exist dist mkdir dist
%ISCC% setup\classsend2.iss
if errorlevel 1 goto :err

echo   dist\ClassSend2-Setup-v0.0.1.exe  -- installer for teacher or student PC
goto :eof

:err
echo.
echo Build FAILED.
exit /b 1
