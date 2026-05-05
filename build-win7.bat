@echo off
rem Build the Win7 32-bit agent using Go 1.20 (last release that supports
rem Win7). Run this whenever the agent source changes; the main build.bat
rem just bundles the dist/classsend-agent-win7-x86.exe produced here.
rem
rem Why a separate script: the project's go.mod has `go 1.24.2`, which Go
rem 1.20 refuses to parse. We swap the directive in-place, build, restore.
rem Doing this from inside build.bat would be too fragile — every interruption
rem mid-build would leave the repo with a downgraded go.mod.

setlocal

if not exist "C:\Go120\bin\go.exe" (
    echo ERROR: Go 1.20 not found at C:\Go120
    exit /b 1
)

rem Pull the version from the main build.bat so the Win7 agent reports the
rem same build string as the rest of the release.
for /f "tokens=1,* delims==" %%A in ('findstr /b /c:"set VERSION=" build.bat') do set VERSION=%%B
if not "%VERSION_OVERRIDE%"=="" set VERSION=%VERSION_OVERRIDE%
for /f "usebackq delims=" %%I in (`powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-ddTHH:mm:ss'"`) do set BUILDTIME=%%I
set VFLAGS=-X classsend/internal/buildinfo.Version=%VERSION% -X classsend/internal/buildinfo.BuildTime=%BUILDTIME% %RELEASE_FLAGS%

echo Building classsend-agent-win7-x86.exe (v%VERSION%, Go 1.20, GOARCH=386)...

rem Back up go.mod / go.sum, downgrade the version directive for Go 1.20.
copy /Y go.mod go.mod.bak >nul
copy /Y go.sum go.sum.bak >nul
powershell -NoProfile -Command "(Get-Content go.mod) -replace '^go 1\.24\.2$','go 1.20' | Set-Content go.mod"

set "GOROOT=C:\Go120"
set "GOOS=windows"
set "GOARCH=386"
set "GOTOOLCHAIN=local"
set "PATH=C:\Go120\bin;%PATH%"

if not exist dist mkdir dist
"C:\Go120\bin\go.exe" build -ldflags="-H windowsgui %VFLAGS%" -o dist\classsend-agent-win7-x86.exe ./cmd/classsend-agent
set BUILD_RC=%errorlevel%

rem Always restore go.mod / go.sum, even on build failure.
move /Y go.mod.bak go.mod >nul
move /Y go.sum.bak go.sum >nul

if not %BUILD_RC%==0 (
    echo Build FAILED.
    exit /b %BUILD_RC%
)

echo.
echo   dist\classsend-agent-win7-x86.exe   -- 32-bit agent for Windows 7/8 PCs
echo.
echo Note: the student TUI (student.exe) currently requires Go 1.21+ because
echo bubbletea's transitive deps now import the slices stdlib package. On
echo Win7, only the agent is shipped; the chat TUI is not available there.
goto :eof
