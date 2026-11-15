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
# No administrator rights are needed.
#
# The zip is checked against checksums.txt of the same release, both fetched
# over HTTPS from GitHub. The signature of checksums.txt is not checked here:
# to verify it too, follow the manual install in
# docs/install/worker-windows-preview.md.
#
# Environment (all optional):
#   MINION_VERSION      the release to install (v0.8.1 or 0.8.1); default: latest
#   MINION_INSTALL_DIR  where to put minion.exe instead of the default above
#   MINION_RELEASE_BASE, MINION_RELEASE_API
#                       where archives and the latest release are looked up
#                       (mirrors, tests)
#
# Runs in Windows PowerShell 5.1 and in PowerShell 7. Errors are thrown, not
# "exit": with "irm | iex" an exit would close the user's window.

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is slow with a progress bar in 5.1
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $repo = 'patrick-1984/ai-minion'

    # A 32-bit PowerShell on a 64-bit Windows reports x86 here and the real
    # architecture in PROCESSOR_ARCHITEW6432.
    $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($cpu) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "install.ps1: unsupported architecture '$cpu' (supported: AMD64, ARM64)" }
    }

    $ver = $env:MINION_VERSION
    if (-not $ver) {
        $api = if ($env:MINION_RELEASE_API) { $env:MINION_RELEASE_API } else { "https://api.github.com/repos/$repo/releases" }
        $ver = (Invoke-RestMethod -UseBasicParsing -Uri "$api/latest").tag_name
        if (-not $ver) { throw "install.ps1: could not read the latest release from $api/latest" }
    }
    if (-not $ver.StartsWith('v')) { $ver = "v$ver" }
    if ($ver -notmatch '^v\d+\.\d+\.\d+') { throw "install.ps1: invalid MINION_VERSION '$ver': give an exact version such as v0.8.1" }

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

        $dir = if ($env:MINION_INSTALL_DIR) { $env:MINION_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\minion' }
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        try {
            Copy-Item -Force -LiteralPath $exe -Destination (Join-Path $dir 'minion.exe')
        } catch {
            throw "install.ps1: cannot write $dir\minion.exe (is a Worker running? stop it first): $_"
        }
        Write-Host "Installed $dir\minion.exe"

        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        # @() keeps a PATH with one entry a list, so that + appends an element.
        $parts = @($userPath -split ';' | Where-Object { $_ })
        if ($parts -notcontains $dir) {
            [Environment]::SetEnvironmentVariable('Path', (($parts + $dir) -join ';'), 'User')
            Write-Host "Added $dir to your user PATH (new terminals see it)."
        }
        if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }

        & (Join-Path $dir 'minion.exe') version
    } finally {
        Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
    }
}
