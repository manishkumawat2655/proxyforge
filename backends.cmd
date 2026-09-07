@echo off
setlocal EnableExtensions
rem ---------------------------------------------------------------------------
rem backends.cmd -- start/stop the dummy backends ProxyForge balances across.
rem
rem Works in cmd.exe AND PowerShell, and finds go.exe itself.
rem
rem   backends start      start three backends on 9001, 9002, 9003
rem   backends stop       stop all of them
rem   backends start 9004 start one extra on a specific port
rem ---------------------------------------------------------------------------

set "ROOT=%~dp0"
set "EXE=%ROOT%.logs\dummybackend.exe"

if /I "%~1"=="stop"  goto :stop
if /I "%~1"=="start" goto :start
goto :usage

rem ---------------------------------------------------------------------------
:start
call :find_go
if not defined GOEXE goto :no_go

if not exist "%ROOT%.logs" mkdir "%ROOT%.logs"

rem Rebuild every time is fine here: `go build` is cached and fast, and a
rem running dummybackend.exe only locks the file while it is up -- which is why
rem we tolerate a build failure if one is already running.
pushd "%ROOT%"
"%GOEXE%" build -o ".logs\dummybackend.exe" ./cmd/dummybackend 2>nul
popd

if not exist "%EXE%" (
    echo backends: build failed and no existing binary to fall back on.
    exit /b 1
)

rem A specific port was requested.
if not "%~2"=="" (
    call :spawn %~2
    goto :eof
)

call :spawn 9001
call :spawn 9002
call :spawn 9003

echo.
echo Try:  curl.exe http://127.0.0.1:9001/health
echo Stop: backends stop
goto :eof

rem ---------------------------------------------------------------------------
rem `start` with /min gives each backend its own minimised window, so you can
rem see its log and close it by hand if you want.
:spawn
start "backend-%~1" /min "%EXE%" -port %~1
echo started backend on http://127.0.0.1:%~1
exit /b 0

rem ---------------------------------------------------------------------------
:stop
taskkill /IM dummybackend.exe /F >nul 2>&1
if errorlevel 1 (
    echo no dummy backends were running
) else (
    echo stopped all dummy backends
)
goto :eof

rem ---------------------------------------------------------------------------
:find_go
set "GOEXE="
where go >nul 2>&1
if not errorlevel 1 (
    set "GOEXE=go"
    exit /b 0
)
if exist "%ProgramFiles%\Go\bin\go.exe"           set "GOEXE=%ProgramFiles%\Go\bin\go.exe" & exit /b 0
if exist "%ProgramFiles(x86)%\Go\bin\go.exe"      set "GOEXE=%ProgramFiles(x86)%\Go\bin\go.exe" & exit /b 0
if exist "%SystemDrive%\Go\bin\go.exe"            set "GOEXE=%SystemDrive%\Go\bin\go.exe" & exit /b 0
if exist "%LOCALAPPDATA%\Programs\Go\bin\go.exe"  set "GOEXE=%LOCALAPPDATA%\Programs\Go\bin\go.exe" & exit /b 0
exit /b 1

rem ---------------------------------------------------------------------------
:no_go
echo backends: could not find go.exe.
echo Install it with:  winget install --id GoLang.Go -e
exit /b 1

rem ---------------------------------------------------------------------------
:usage
echo Usage:
echo     backends start        start three backends on 9001-9003
echo     backends start 9004   start one extra backend on port 9004
echo     backends stop         stop all dummy backends
exit /b 2
