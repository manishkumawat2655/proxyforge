@echo off
setlocal EnableExtensions
rem ---------------------------------------------------------------------------
rem test.cmd -- run the test suite without caring about PATH or which shell.
rem
rem   test          run all tests
rem   test race     run all tests with the race detector (needs gcc)
rem ---------------------------------------------------------------------------

set "ROOT=%~dp0"

call :find_go
if not defined GOEXE goto :no_go

pushd "%ROOT%"

if /I "%~1"=="race" (
    echo Running tests with the race detector...
    echo.
    "%GOEXE%" test -race ./...
    set "RC=%ERRORLEVEL%"
    if not "%RC%"=="0" call :race_hint
) else (
    "%GOEXE%" test ./...
    set "RC=%ERRORLEVEL%"
)

popd
exit /b %RC%

rem ---------------------------------------------------------------------------
rem The race detector is built on cgo. With no C compiler on PATH, Go sets
rem CGO_ENABLED=0 and refuses to run it. Plain `test` is unaffected.
:race_hint
where gcc >nul 2>&1
if errorlevel 1 (
    echo.
    echo test: no C compiler found, which the race detector requires.
    echo test: install one with:
    echo test:     winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e
    echo test: then reopen your terminal. Plain `test` works without it.
)
exit /b 0

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
echo test: could not find go.exe.
echo Install it with:  winget install --id GoLang.Go -e
exit /b 1
