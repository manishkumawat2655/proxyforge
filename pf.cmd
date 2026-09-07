@echo off
setlocal EnableExtensions
rem ---------------------------------------------------------------------------
rem pf.cmd -- run ProxyForge without caring about PATH or which shell you are in.
rem
rem Works in cmd.exe AND PowerShell. Finds go.exe itself, builds the binary on
rem first use, then forwards every argument to it.
rem
rem   pf start --config config.yaml
rem   pf status
rem   pf add-backend --url http://127.0.0.1:9004 --weight 2
rem   pf build            <- force a rebuild after you change the source
rem ---------------------------------------------------------------------------

set "ROOT=%~dp0"
set "EXE=%ROOT%.logs\proxyforge.exe"

call :find_go
if not defined GOEXE goto :no_go

if not exist "%ROOT%.logs" mkdir "%ROOT%.logs"

rem "pf build" forces a rebuild; otherwise we only build when the exe is missing.
rem We do NOT rebuild on every call, because Windows locks a running .exe and the
rem build would fail while `pf start` is up in another window.
if /I "%~1"=="build" (
    call :build
    if errorlevel 1 exit /b 1
    echo pf: built %EXE%
    exit /b 0
)

if not exist "%EXE%" (
    call :build
    if errorlevel 1 exit /b 1
)

"%EXE%" %*
exit /b %ERRORLEVEL%

rem ---------------------------------------------------------------------------
:build
pushd "%ROOT%"
"%GOEXE%" build -o ".logs\proxyforge.exe" ./cmd/proxyforge
set "BUILD_RC=%ERRORLEVEL%"
popd
if not "%BUILD_RC%"=="0" (
    echo.
    echo pf: build failed.
    echo pf: if ProxyForge is already running, stop it first -- Windows locks a
    echo pf: running .exe and it cannot be overwritten.
)
exit /b %BUILD_RC%

rem ---------------------------------------------------------------------------
rem Look for go.exe on PATH first, then in the usual install locations. This is
rem what makes the script survive a terminal that was opened before Go was
rem installed and is still holding a stale PATH.
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
echo pf: could not find go.exe.
echo.
echo Install it with:
echo     winget install --id GoLang.Go -e
echo.
echo If Go IS already installed, this script looked in:
echo     PATH
echo     %ProgramFiles%\Go\bin
echo     %SystemDrive%\Go\bin
echo     %LOCALAPPDATA%\Programs\Go\bin
exit /b 1
