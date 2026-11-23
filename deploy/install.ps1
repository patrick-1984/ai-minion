# install.ps1: the one-line installer of minion.exe for Windows (preview).
#
#     [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; irm https://github.com/patrick-1984/ai-minion/releases/latest/download/install.ps1 | iex
#
# (The TLS 1.2 line is for Windows PowerShell 5.1 with old TLS defaults.)
#
# It finds the latest release (or $env:MINION_VERSION), downloads the Windows
# preview zip for this machine (amd64 or arm64) and the release's
# checksums.txt, checks the zip's SHA-256 against checksums.txt, puts
# minion.exe into "$env:LOCALAPPDATA\Programs\minion", adds that directory to
# the PATH of the current user when it is missing, and prints "minion version".
# When psmux is missing, WinGet installs it for the Worker's sessions.
# No administrator rights are needed.
#
# The zip is checked against checksums.txt of the same release, both fetched
# over HTTPS from GitHub. The signature of checksums.txt is not checked here:
# to verify it too, follow the manual install in
# docs/install/worker-windows-preview.md.
#
# Environment (all optional):
#   MINION_JOIN         with -Join: the join token (else a hidden prompt asks)
#   MINION_VERSION      the release to install (v0.8.1 or 0.8.1); default: latest
#   MINION_INSTALL_DIR  where to put minion.exe instead of the default above
#   MINION_RELEASE_BASE, MINION_RELEASE_API
#                       where archives and the latest release are looked up
#                       (mirrors, tests)
#
# Runs in Windows PowerShell 5.1 and in PowerShell 7. Errors are thrown, not
# "exit": with "irm | iex" an exit would close the user's window.

param(
    [switch]$Join,
    [switch]$AcceptUsage,
    [string]$Version,
    [string]$Name,
    [ValidateSet('all', 'none', 'later')][string]$Share = 'all'
)

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is slow with a progress bar in 5.1
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $token = $null
    if ($AcceptUsage) {
        if (-not $Join) { throw 'install.ps1: -AcceptUsage requires -Join' }
        Write-Host 'Usage monitoring for this enrollment'
        Write-Host 'The Hub administrator and Worker owner can see usage for every session, including unshared sessions.'
        Write-Host 'Reports may include session names, folders, models, account identifiers, timing, token counts, costs and usage limits.'
        Write-Host 'This grants no terminal content or control access. Sharing and bucket choices do not change usage coverage.'
    }
    if ($Join) {
        if ($env:MINION_JOIN) {
            # The Hub's command puts the token here; read it once and clear it.
            $token = $env:MINION_JOIN
            Remove-Item Env:MINION_JOIN -ErrorAction SilentlyContinue
        } else {
            $secret = Read-Host 'Join token from the Hub Add worker page' -AsSecureString
            $buffer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secret)
            try { $token = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($buffer) }
            finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($buffer); $secret.Dispose() }
        }
        if ($token -notmatch '^mnj1-[A-Za-z0-9_.-]+$') { throw 'install.ps1: paste the full join token from the Hub Add worker page' }
    }
    $repo = 'patrick-1984/ai-minion'

    # A 32-bit PowerShell on a 64-bit Windows reports x86 here and the real
    # architecture in PROCESSOR_ARCHITEW6432.
    $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($cpu) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "install.ps1: unsupported architecture '$cpu' (supported: AMD64, ARM64)" }
    }

    $ver = if ($Version) { $Version } else { $env:MINION_VERSION }
    if (-not $ver) {
        $api = if ($env:MINION_RELEASE_API) { $env:MINION_RELEASE_API } else { "https://api.github.com/repos/$repo/releases" }
        $ver = (Invoke-RestMethod -UseBasicParsing -Uri "$api/latest").tag_name
        if (-not $ver) { throw "install.ps1: could not read the latest release from $api/latest" }
    }
    if (-not $ver.StartsWith('v')) { $ver = "v$ver" }
    if ($ver -notmatch '^v\d+\.\d+\.\d+(?:-[a-zA-Z0-9.-]+)?$') { throw "install.ps1: invalid MINION_VERSION '$ver': give an exact version such as v0.8.1" }

    $zip = "minion_$($ver.Substring(1))_windows_$($arch)_preview.zip"
    $baseRoot = if ($env:MINION_RELEASE_BASE) { $env:MINION_RELEASE_BASE } else { "https://github.com/$repo/releases/download" }
    $base = "$baseRoot/$ver"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("minion-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading $zip ($ver)"
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$zip" -OutFile (Join-Path $tmp $zip)
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

        $want = $null
        foreach ($line in Get-Content -LiteralPath (Join-Path $tmp 'checksums.txt')) {
            $f = $line -split '\s+'
            if ($f.Count -ge 2 -and ($f[1] -eq $zip -or $f[1] -eq "*$zip")) { $want = $f[0].ToLower(); break }
        }
        if (-not $want) { throw "install.ps1: $zip is not listed in checksums.txt of $ver" }
        $got = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $zip)).Hash.ToLower()
        if ($got -ne $want) { throw "install.ps1: checksum mismatch for ${zip}: got $got, checksums.txt says $want; nothing was installed" }

        $unpacked = Join-Path $tmp 'unpacked'
        Expand-Archive -LiteralPath (Join-Path $tmp $zip) -DestinationPath $unpacked -Force
        $exe = Join-Path $unpacked 'minion.exe'
        if (-not (Test-Path -LiteralPath $exe)) { throw "install.ps1: $zip holds no minion.exe" }

        $workerArgs = @('worker', 'up', '--join', '-')
        if ($Name) { $workerArgs += @('--name', $Name) }
        if ($Join) {
            $consent = $token | & $exe @workerArgs --prepare-join
            if ($LASTEXITCODE -ne 0) { throw "install.ps1: Worker inspection failed (exit $LASTEXITCODE)" }
            if ($consent -eq 'minion: join skipped: existing Worker kept') { return }
            if ($consent -notmatch '^[0-9a-f]{64}$') { throw 'install.ps1: could not get the Worker profile decision' }
        }

        # A newly installed WinGet link may not be on this PowerShell's PATH yet.
        $psmuxLink = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links\psmux.exe'
        function Find-Psmux {
            (Get-Command 'psmux.exe' -CommandType Application -ErrorAction SilentlyContinue) -or
                (Test-Path -LiteralPath $psmuxLink -PathType Leaf)
        }
        # psmux is offered on a fresh install only: an existing Worker state
        # directory may hold a decision (psmux_offer) to run without it.
        $stateDir = if ($env:MINION_STATE_DIR) { $env:MINION_STATE_DIR } else { Join-Path $env:LOCALAPPDATA 'minion\state' }
        $freshInstall = -not (Test-Path -LiteralPath $stateDir) -or -not (Get-ChildItem -LiteralPath $stateDir -Force -ErrorAction SilentlyContinue)
        if ($env:MINION_SESSION_HOST -ne 'conpty' -and $freshInstall -and -not (Find-Psmux)) {
            if (-not (Get-Command 'winget.exe' -CommandType Application -ErrorAction SilentlyContinue)) {
                Write-Warning 'install.ps1: psmux and WinGet are missing. The Worker will use ConPTY sessions. To use psmux, install it from https://github.com/psmux/psmux/releases.'
            } else {
                Write-Host 'Installing psmux with WinGet'
                & winget.exe install --id marlocarlo.psmux --exact --source winget --accept-source-agreements --accept-package-agreements --disable-interactivity
                if ($LASTEXITCODE -ne 0) {
                    Write-Warning "install.ps1: WinGet failed ($LASTEXITCODE). The Worker will use ConPTY sessions. Run 'winget install psmux' later to use psmux."
                } else {
                    # The running PowerShell still has the old PATH: read it again.
                    $env:Path = (@([Environment]::GetEnvironmentVariable('Path', 'Machine'), [Environment]::GetEnvironmentVariable('Path', 'User')) | Where-Object { $_ }) -join ';'
                    if (-not (Find-Psmux)) {
                        Write-Warning 'install.ps1: WinGet finished but psmux.exe was not found. The Worker will use ConPTY sessions; open a new terminal later to pick psmux up.'
                    }
                }
            }
        }

        $dir = if ($env:MINION_INSTALL_DIR) { $env:MINION_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\minion' }
        if ($Join) {
            # The incumbent may still be using its executable. Keep the
            # candidate at a separate path so rollback can restart the old one.
            $dir = Join-Path $dir "releases\$ver-$($got.Substring(0, 12))"
        }
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $installed = Join-Path $dir 'minion.exe'
        try {
            $alreadyInstalled = (Test-Path -LiteralPath $installed -PathType Leaf) -and
                ((Get-FileHash -Algorithm SHA256 -LiteralPath $installed).Hash -eq (Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash)
            if (-not $alreadyInstalled) { Copy-Item -Force -LiteralPath $exe -Destination $installed }
        } catch {
            throw "install.ps1: cannot write $installed (is a Worker running? stop it first): $_"
        }
        Write-Host "Installed $installed"

        & $installed version
        if ($Join) {
            if ($AcceptUsage) { $workerArgs += '--accept-usage' }
            $token | & $installed @workerArgs --join-consent $consent --share-existing $Share
            $token = $null
            if ($LASTEXITCODE -ne 0) { throw "install.ps1: Worker setup failed (exit $LASTEXITCODE)" }
        }

        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        # @() keeps a PATH with one entry a list, so that + appends an element.
        $parts = @($userPath -split ';' | Where-Object { $_ })
        if ($parts -notcontains $dir) {
            [Environment]::SetEnvironmentVariable('Path', (($parts + $dir) -join ';'), 'User')
            Write-Host "Added $dir to your user PATH (new terminals see it)."
        }
        if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }

    } finally {
        Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
    }
}
