# run-backends.ps1 -- start N dummy backends in the background for local testing.
#
#   .\scripts\run-backends.ps1                    # ports 9001, 9002, 9003
#   .\scripts\run-backends.ps1 -Ports 9001,9002   # just two
#
# Each backend's output lands in .logs\backend-<port>.err.log. Note that Go's
# `log` package writes to STDERR, so the "listening on ..." lines go to the
# .err.log file, not the .out.log one -- that is normal, not a failure.
#
# Stop them again with .\scripts\stop-backends.ps1

param(
    [int[]]$Ports = @(9001, 9002, 9003)
)

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
$logDir = Join-Path $root ".logs"
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

Push-Location $root
try {
    # Build once, up front. If we let three `go run` invocations start
    # simultaneously they would each compile the same package and contend on the
    # build cache -- harmless but slow and noisy. One binary, three processes.
    $exe = Join-Path $logDir "dummybackend.exe"
    Write-Host "building $exe ..."
    go build -o $exe ./cmd/dummybackend
    if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)" }

    foreach ($p in $Ports) {
        $args = @("-port", "$p")
        Start-Process -FilePath $exe `
            -ArgumentList $args `
            -RedirectStandardOutput (Join-Path $logDir "backend-$p.out.log") `
            -RedirectStandardError  (Join-Path $logDir "backend-$p.err.log") `
            -WindowStyle Hidden | Out-Null
        Write-Host "started backend on http://127.0.0.1:$p"
    }

    Write-Host ""
    Write-Host "try:  curl.exe http://127.0.0.1:$($Ports[0])/health"
    Write-Host "logs: Get-Content $logDir\backend-$($Ports[0]).err.log -Wait"
}
finally {
    Pop-Location
}
