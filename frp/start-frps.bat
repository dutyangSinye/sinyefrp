@echo off
setlocal

:: Always run relative to this script's directory.
cd /d "%~dp0"

title FRP Server

if not exist "frps.exe" (
    echo [ERROR] frps.exe was not found in:
    echo         %CD%
    pause
    exit /b 1
)

if not exist "frps.toml" (
    echo [ERROR] frps.toml was not found in:
    echo         %CD%
    pause
    exit /b 1
)

:: Stop a previously running FRP server.
taskkill /f /im frps.exe >nul 2>nul

:: Validate configuration before starting the server.
frps.exe verify -c "frps.toml"
if errorlevel 1 (
    echo.
    echo [ERROR] frps.toml validation failed.
    pause
    exit /b 1
)

echo.
echo Starting FRP server...
echo Press Ctrl+C to stop it.
echo.

frps.exe -c "frps.toml"
set "FRPS_EXIT_CODE=%ERRORLEVEL%"

echo.
echo FRP server stopped with exit code %FRPS_EXIT_CODE%.
pause
exit /b %FRPS_EXIT_CODE%
