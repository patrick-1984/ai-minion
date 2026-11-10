# windows.ps1: run the exact release zip on a clean Windows runner (Story 9.7,
# AD-24, R98). The Windows build is a preview.
#
# Usage: pwsh test/smoke/windows.ps1 -Assets <dir> -Tag vX.Y.Z[-rc.N]
#
# Checks: the preview zip hashes to a line of the signed checksums.txt (the
# workflow verified the cosign bundle before), "minion version" prints the tag,
# and a Worker runs in the foreground ("worker run") with a throw-away state
# directory: it must stay up, report that its core started (the Windows session
# host is ConPTY, AD-13, constructed at Worker start) and "minion worker
# status" must show the version and the preview-only flag.
#
# Not covered here (deferred; tracked by the GitHub issue "Install smoke (Windows): drive a
# ConPTY session through the released binary"): a ConPTY
# session driven through the released binary. The Windows Worker has no local
# control socket yet (the named pipe is Story 8.2) and no CLI command creates a
# session, so a session can only be driven through a Hub. Step to add before
# v0.1.0-rc.1: drive a ConPTY session over the pipe. The real ConPTY sessions
# are exercised by internal/worker/term/conpty on the Windows runner
# (windows.yml).
#
# The Go test TestWindowsSmokeArgvAndEnvironmentRunOnLinux extracts the "worker
# run" argument list and the $env:MINION_* assignments of this file and runs
# the real binary with them on Linux.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Assets,
    [Parameter(Mandatory = $true)][string]$Tag
)
$ErrorActionPreference = 'Stop'

function Fail([string]$Message) {
    Write-Host "smoke: FAIL: $Message"
    exit 1
}

if ($Tag -notmatch '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$') {
    Fail "'$Tag' is not a release tag (vX.Y.Z or vX.Y.Z-rc.N)"
}
$version = $Tag.Substring(1)
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$archive = "minion_${version}_windows_${arch}_preview.zip"
$zip = Join-Path $Assets $archive
if (-not (Test-Path $zip)) { Fail "missing $archive in $Assets" }

# The archive must be one of the files of the signed checksums.txt.
$hash = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLowerInvariant()
$listed = Get-Content (Join-Path $Assets 'checksums.txt') | Where-Object { $_ -match "^$hash\s" }
if (-not $listed) { Fail "$archive does not match the signed checksums.txt" }
Write-Host "smoke: $archive matches checksums.txt"

$work = Join-Path ([System.IO.Path]::GetTempPath()) ("minion-smoke-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$worker = $null
try {
    Expand-Archive -Path $zip -DestinationPath (Join-Path $work 'bin')
    $minion = Join-Path $work 'bin\minion.exe'
    if (-not (Test-Path $minion)) { Fail "$archive holds no minion.exe" }

    $versionOut = & $minion version
    if ($LASTEXITCODE -ne 0) { Fail "minion version exited with $LASTEXITCODE" }
    if (($versionOut -split "`r?`n") -notcontains "version: $Tag") {
        Fail "minion version printed '$($versionOut | Select-Object -First 1)', expected 'version: $Tag'"
    }
    Write-Host "smoke: $($versionOut | Select-Object -First 1)"

    # A foreground Worker with a throw-away state directory (no Hub: Solo).
    $env:MINION_STATE_DIR = Join-Path $work 'state'
    New-Item -ItemType Directory -Path $env:MINION_STATE_DIR | Out-Null
    $log = Join-Path $work 'worker.log'
    # The preview-only setting is a switch flag, so it goes through the environment:
    # both "worker run" and "worker status" inherit it (a flag would reach only
    # the Worker, and "true" would become a positional argument).
    $env:MINION_PREVIEW_ONLY = 'true'
    $worker = Start-Process -FilePath $minion -ArgumentList @('worker', 'run') `
        -RedirectStandardError $log -RedirectStandardOutput (Join-Path $work 'worker.out') -PassThru -NoNewWindow

    $deadline = (Get-Date).AddSeconds(30)
    $started = $false
    while ((Get-Date) -lt $deadline) {
        if ($worker.HasExited) { Fail "the Worker exited with $($worker.ExitCode): $(Get-Content $log -Raw)" }
        if ((Test-Path $log) -and ((Get-Content $log -Raw) -match 'worker core started')) { $started = $true; break }
        Start-Sleep -Seconds 1
    }
    if (-not $started) { Fail "the Worker did not report that it started: $(Get-Content $log -Raw)" }
    Start-Sleep -Seconds 5
    if ($worker.HasExited) { Fail "the Worker exited with $($worker.ExitCode): $(Get-Content $log -Raw)" }

    $status = (& $minion worker status) -join "`n"
    if ($LASTEXITCODE -ne 0) { Fail "minion worker status exited with $LASTEXITCODE" }
    if ($status -notmatch "(?m)^Version:\s+$([regex]::Escape($Tag))\s*$") { Fail "worker status shows another version: $status" }
    if ($status -notmatch '(?m)^Preview only:\s+yes\s*$') { Fail "worker status does not show the preview-only flag: $status" }
    Write-Host 'smoke: windows smoke test passed'
}
finally {
    if ($worker -and -not $worker.HasExited) { Stop-Process -Id $worker.Id -Force }
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
