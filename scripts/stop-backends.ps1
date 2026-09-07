# stop-backends.ps1 -- kill every dummy backend started by run-backends.ps1.
#
# This is a blunt Stop-Process, i.e. the opposite of the graceful shutdown we
# build in Phase 8. That contrast is the point: right now in-flight requests are
# simply severed. By Phase 8 you will be able to explain exactly what a client
# sees when that happens.

$procs = Get-Process -Name "dummybackend" -ErrorAction SilentlyContinue

if (-not $procs) {
    Write-Host "no dummybackend processes running"
    return
}

foreach ($p in $procs) {
    Stop-Process -Id $p.Id -Force
    Write-Host "stopped pid $($p.Id)"
}
