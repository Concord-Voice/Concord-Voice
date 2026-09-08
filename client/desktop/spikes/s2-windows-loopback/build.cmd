@echo off
REM S2 probe build. Run from a "x64 Native Tools Command Prompt for VS 2022"
REM (Start menu -> Visual Studio 2022 -> x64 Native Tools Command Prompt).
REM
REM No SDK version floor: s2probe.cpp declares the process-loopback activation
REM structs locally instead of including <audioclientactivationparams.h>, so any
REM SDK from ~10.0.17763 upward builds it. That is deliberate -- the probe's job
REM is to measure the RUNTIME floor, and gating the build on the SDK would hide
REM the answer. See ADR-0043 risk 3.

where cl.exe >nul 2>&1
if errorlevel 1 (
  echo.
  echo   cl.exe is not on PATH.
  echo   Open "x64 Native Tools Command Prompt for VS 2022" and run this again.
  echo.
  exit /b 1
)

cl /nologo /EHsc /std:c++17 /W4 /O2 s2probe.cpp /Fe:s2probe.exe ^
   /link ole32.lib mmdevapi.lib user32.lib

if errorlevel 1 (
  echo.
  echo   BUILD FAILED. Paste the error above into the session; do not work around it.
  echo.
  exit /b 1
)

del /q s2probe.obj 2>nul

echo.
echo   Built s2probe.exe
echo.
echo   Next:  s2probe.exe --list
echo          s2probe.exe --hwnd 0x00000000000A1B2C --seconds 8
echo.
