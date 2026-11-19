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
#   MINION_INSTALL_SERVICE=1
#                       install the Worker as the Windows service instead
#                       (needs an elevated PowerShell): the zip is downloaded,
#                       checked and unpacked in a new directory below
#                       Program Files that only administrators and SYSTEM can
#                       open, never in your profile, and "minion worker up" runs
#                       from there. It installs the signed release, verified,
#                       into %ProgramFiles%\Minion and starts the service; the
#                       bootstrap directory is removed afterwards. Nothing is
#                       put in your profile or PATH. Pair the Worker first
#                       ("minion worker pair") so that the saved settings
#                       apply.
#   MINION_SERVICE_ARGS more arguments for "worker up" in service mode,
#                       separated by spaces (for example
#                       "--share-existing none")
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
    $service = $env:MINION_INSTALL_SERVICE -eq '1'
    if ($service) {
        $principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
        if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
            throw "install.ps1: MINION_INSTALL_SERVICE=1 installs a Windows service: run it in a PowerShell started as administrator"
        }
        # A new directory below Program Files (where only administrators may
        # create anything), closed to everyone but SYSTEM and the
        # administrators before anything is put in it: the binary that runs
        # elevated never lies where a user's program could change it.
        $pf = [Environment]::GetFolderPath('ProgramFiles')
        $tmp = Join-Path $pf ("Minion.bootstrap-" + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $tmp | Out-Null
        $acl = New-Object Security.AccessControl.DirectorySecurity
        $acl.SetSecurityDescriptorSddlForm('O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)')
        Set-Acl -LiteralPath $tmp -AclObject $acl
    } else {
        $tmp = Join-Path ([IO.Path]::GetTempPath()) ("minion-install-" + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $tmp | Out-Null
    }
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

        if ($service) {
            # Only a release that installs the protected service (it fetches
            # and verifies its own signed release into %ProgramFiles%\Minion)
            # may be used for it; an older one would register this directory.
            $help = (& $exe worker up --help 2>&1) -join "`n"
            if ($help -notmatch 'allow-unverified-service-binary') {
                throw "install.ps1: $ver does not install the Windows service into a protected directory; use a newer release (MINION_VERSION)"
            }
            $svcArgs = @()
            if ($env:MINION_SERVICE_ARGS) { $svcArgs = @($env:MINION_SERVICE_ARGS -split '\s+' | Where-Object { $_ }) }
            Write-Host "Running $exe worker up $($svcArgs -join ' ')"
            & $exe worker up @svcArgs
            if ($LASTEXITCODE -ne 0) { throw "install.ps1: minion worker up failed with exit status $LASTEXITCODE" }
            return
        }

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
