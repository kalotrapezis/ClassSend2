@echo off
rem fetch-ffmpeg.bat — downloads BtbN's pre-built static ffmpeg.exe into
rem third_party\ffmpeg\ so build.bat can bundle it into the teacher
rem installer's "Teacher Screen Casting" component.
rem
rem Idempotent: if ffmpeg.exe already exists, exits without doing anything.
rem Run after a fresh clone, or any time you want to refresh to the latest
rem nightly. The download is ~210 MB and may take a couple of minutes.
rem
rem Source:  https://github.com/BtbN/FFmpeg-Builds/releases/latest
rem Asset:   ffmpeg-master-latest-win64-gpl.zip
rem License: GPL 3 (libx264 is GPL — LICENSE.txt extracted alongside).

setlocal

set "DEST_DIR=third_party\ffmpeg"
set "DEST_EXE=%DEST_DIR%\ffmpeg.exe"
set "URL=https://github.com/BtbN/FFmpeg-Builds/releases/latest/download/ffmpeg-master-latest-win64-gpl.zip"
set "TMP_ZIP=%TEMP%\classsend-ffmpeg-fetch.zip"
set "TMP_EXTRACT=%TEMP%\classsend-ffmpeg-extract"

if exist "%DEST_EXE%" (
    echo %DEST_EXE% already present — skipping.
    echo If you want to refresh, delete it and re-run this script.
    goto :eof
)

if not exist "%DEST_DIR%" mkdir "%DEST_DIR%"

echo Downloading %URL%
echo  to %TMP_ZIP% ^(~210 MB^)...
powershell -NoProfile -Command "$ProgressPreference='SilentlyContinue'; Invoke-WebRequest -Uri '%URL%' -OutFile '%TMP_ZIP%' -UseBasicParsing"
if errorlevel 1 (
    echo Download FAILED.
    exit /b 1
)

echo Extracting ffmpeg.exe + LICENSE.txt...
if exist "%TMP_EXTRACT%" rmdir /s /q "%TMP_EXTRACT%"
powershell -NoProfile -Command "Expand-Archive -Path '%TMP_ZIP%' -DestinationPath '%TMP_EXTRACT%' -Force"
if errorlevel 1 (
    echo Extract FAILED.
    exit /b 1
)

rem BtbN nests everything under ffmpeg-master-latest-win64-gpl\
for /d %%D in ("%TMP_EXTRACT%\ffmpeg-master-latest-win64-gpl*") do set "EXTRACTED=%%D"
if not defined EXTRACTED (
    echo Could not locate the expected ffmpeg-master-latest-win64-gpl directory in the archive.
    exit /b 1
)

copy /Y "%EXTRACTED%\bin\ffmpeg.exe" "%DEST_DIR%\ffmpeg.exe" >nul
if errorlevel 1 goto :err
copy /Y "%EXTRACTED%\LICENSE.txt"   "%DEST_DIR%\LICENSE.txt" >nul
if errorlevel 1 goto :err

del /q "%TMP_ZIP%"
rmdir /s /q "%TMP_EXTRACT%"

echo.
echo Done:
echo   %DEST_DIR%\ffmpeg.exe
echo   %DEST_DIR%\LICENSE.txt
echo.
echo You can now run build.bat — the installer will include the "Teacher Screen
echo Casting" component, checked by default.
goto :eof

:err
echo Copy FAILED. Leaving %TMP_EXTRACT% in place for inspection.
exit /b 1
