# windows-runas.ps1: run a command line as another Windows principal, without
# a window or a password prompt, for the job worker-service of
# .github/workflows/windows.yml (tray ticket W1b, acceptance 3). Dot-source it:
#
#   . scripts/windows-runas.ps1
#   $code = Invoke-AsSaferNormalUser -CommandLine 'C:\minion-ci\minion.exe worker stop --json' -Out out.json -Err err.txt
#   $cred = New-CiLocalUser -Name minion-std
#   $code = Invoke-AsCredential -Credential $cred -CommandLine '...' -Out out.json -Err err.txt
#
# Both run "cmd.exe /d /s /c <command line> >out 2>err" and return the exit
# status of the command line. The output files must be in a directory the
# principal can write (C:\minion-ci in the workflow).
#
# Invoke-AsSaferNormalUser is what "runas /trustlevel:0x20000" does, without
# runas (which opens a new console and does not report the exit status): the
# token of this process restricted to the SAFER level "Normal User"
# (SaferComputeTokenFromLevel). It is the same user SID with the
# Administrators group set to deny-only and the administrator privileges
# removed, so the installer's own pipe admits it and the service control
# manager treats it as a standard user. It works whether UAC is on or off.
#
# Invoke-AsCredential logs on as another local account with its password
# (CreateProcessWithLogonW through .NET Process, LOGON_WITH_PROFILE). The new
# process gets the environment of that user's profile, not this one.

if (-not ('MinionCi.Safer' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;

namespace MinionCi {
public static class Safer {
    const uint SAFER_SCOPEID_USER = 2;
    const uint SAFER_LEVELID_NORMALUSER = 0x20000;
    const uint SAFER_LEVEL_OPEN = 1;
    const uint CREATE_NO_WINDOW = 0x08000000;
    const uint INFINITE = 0xFFFFFFFF;

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    struct STARTUPINFO {
        public int cb; public string lpReserved; public string lpDesktop; public string lpTitle;
        public int dwX; public int dwY; public int dwXSize; public int dwYSize;
        public int dwXCountChars; public int dwYCountChars; public int dwFillAttribute;
        public int dwFlags; public short wShowWindow; public short cbReserved2;
        public IntPtr lpReserved2; public IntPtr hStdInput; public IntPtr hStdOutput; public IntPtr hStdError;
    }
    [StructLayout(LayoutKind.Sequential)]
    struct PROCESS_INFORMATION { public IntPtr hProcess; public IntPtr hThread; public int dwProcessId; public int dwThreadId; }

    [DllImport("advapi32.dll", SetLastError = true)]
    static extern bool SaferCreateLevel(uint scopeId, uint levelId, uint openFlags, out IntPtr level, IntPtr reserved);
    [DllImport("advapi32.dll", SetLastError = true)]
    static extern bool SaferComputeTokenFromLevel(IntPtr level, IntPtr inToken, out IntPtr outToken, uint flags, IntPtr reserved);
    [DllImport("advapi32.dll", SetLastError = true)]
    static extern bool SaferCloseLevel(IntPtr level);
    [DllImport("advapi32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern bool CreateProcessAsUserW(IntPtr token, string app, System.Text.StringBuilder cmd, IntPtr pa, IntPtr ta,
        bool inherit, uint flags, IntPtr env, string cwd, ref STARTUPINFO si, out PROCESS_INFORMATION pi);
    [DllImport("advapi32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
    static extern bool CreateProcessWithTokenW(IntPtr token, uint logonFlags, string app, System.Text.StringBuilder cmd,
        uint flags, IntPtr env, string cwd, ref STARTUPINFO si, out PROCESS_INFORMATION pi);
    [DllImport("kernel32.dll", SetLastError = true)]
    static extern uint WaitForSingleObject(IntPtr h, uint ms);
    [DllImport("kernel32.dll", SetLastError = true)]
    static extern bool GetExitCodeProcess(IntPtr h, out uint code);
    [DllImport("kernel32.dll")]
    static extern bool CloseHandle(IntPtr h);

    // Run runs cmdLine with the SAFER "Normal User" token of this process
    // and returns its exit status. via names the API that started it.
    public static int Run(string app, string cmdLine, string cwd, out string via) {
        IntPtr level, token;
        if (!SaferCreateLevel(SAFER_SCOPEID_USER, SAFER_LEVELID_NORMALUSER, SAFER_LEVEL_OPEN, out level, IntPtr.Zero))
            throw new Win32Exception(Marshal.GetLastWin32Error(), "SaferCreateLevel");
        try {
            if (!SaferComputeTokenFromLevel(level, IntPtr.Zero, out token, 0, IntPtr.Zero))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "SaferComputeTokenFromLevel");
        } finally { SaferCloseLevel(level); }
        try {
            var si = new STARTUPINFO();
            si.cb = Marshal.SizeOf(typeof(STARTUPINFO));
            PROCESS_INFORMATION pi;
            // A restricted version of the caller's own token needs no
            // SeAssignPrimaryTokenPrivilege; CreateProcessWithTokenW (which
            // needs SeImpersonatePrivilege, held by an elevated administrator)
            // is the fallback.
            via = "CreateProcessAsUserW";
            if (!CreateProcessAsUserW(token, app, new System.Text.StringBuilder(cmdLine), IntPtr.Zero, IntPtr.Zero,
                    false, CREATE_NO_WINDOW, IntPtr.Zero, cwd, ref si, out pi)) {
                int first = Marshal.GetLastWin32Error();
                via = "CreateProcessWithTokenW (CreateProcessAsUserW: error " + first + ")";
                si = new STARTUPINFO();
                si.cb = Marshal.SizeOf(typeof(STARTUPINFO));
                if (!CreateProcessWithTokenW(token, 0, app, new System.Text.StringBuilder(cmdLine), CREATE_NO_WINDOW,
                        IntPtr.Zero, cwd, ref si, out pi))
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "CreateProcessWithTokenW after CreateProcessAsUserW error " + first);
            }
            try {
                WaitForSingleObject(pi.hProcess, INFINITE);
                uint code;
                if (!GetExitCodeProcess(pi.hProcess, out code))
                    throw new Win32Exception(Marshal.GetLastWin32Error(), "GetExitCodeProcess");
                return unchecked((int)code);
            } finally { CloseHandle(pi.hThread); CloseHandle(pi.hProcess); }
        } finally { CloseHandle(token); }
    }
}
}
'@
}

# Get-CiCmdArguments returns the cmd.exe argument string that runs $CommandLine with
# its output in $Out and $Err.
function Get-CiCmdArguments([string]$CommandLine, [string]$Out, [string]$Err) {
    return "/d /s /c `"$CommandLine >`"$Out`" 2>`"$Err`"`""
}

function Invoke-AsSaferNormalUser {
    param(
        [Parameter(Mandatory)][string]$CommandLine,
        [Parameter(Mandatory)][string]$Out,
        [Parameter(Mandatory)][string]$Err,
        [string]$WorkingDirectory = 'C:\minion-ci'
    )
    $cmd = Join-Path $env:SystemRoot 'System32\cmd.exe'
    $line = "`"$cmd`" " + (Get-CiCmdArguments $CommandLine $Out $Err)
    $via = ''
    $code = [MinionCi.Safer]::Run($cmd, $line, $WorkingDirectory, [ref]$via)
    Write-Host "[safer normal user via $via] $CommandLine -> exit $code"
    return $code
}

# New-CiLocalUser creates a local account with a random password (net user
# adds it to Users; with -Admin it also joins Administrators) and returns its
# credential. net.exe is used because the LocalAccounts module does not load
# in every PowerShell 7; a password of 14 characters keeps net.exe from asking
# about older Windows versions.
function New-CiLocalUser {
    param([Parameter(Mandatory)][string]$Name, [switch]$Admin)
    $bytes = [byte[]]::new(5)
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    $plain = 'Aa1!' + [Convert]::ToHexString($bytes).ToLower()
    & net.exe user $Name /delete 2>$null | Out-Null
    & net.exe user $Name $plain /add /expires:never | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "net user $Name /add failed ($LASTEXITCODE)" }
    if ($Admin) {
        $group = ([System.Security.Principal.SecurityIdentifier]'S-1-5-32-544').Translate([System.Security.Principal.NTAccount]).Value.Split('\')[-1]
        & net.exe localgroup $group $Name /add | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "net localgroup $group $Name /add failed ($LASTEXITCODE)" }
    }
    $global:LASTEXITCODE = 0
    return [pscredential]::new($Name, (ConvertTo-SecureString $plain -AsPlainText -Force))
}

function Invoke-AsCredential {
    param(
        [Parameter(Mandatory)][pscredential]$Credential,
        [Parameter(Mandatory)][string]$CommandLine,
        [Parameter(Mandatory)][string]$Out,
        [Parameter(Mandatory)][string]$Err,
        [string]$WorkingDirectory = 'C:\minion-ci'
    )
    $psi = [System.Diagnostics.ProcessStartInfo]::new((Join-Path $env:SystemRoot 'System32\cmd.exe'))
    $psi.Arguments = Get-CiCmdArguments $CommandLine $Out $Err
    $psi.UserName = $Credential.UserName
    $psi.Password = $Credential.Password
    $psi.Domain = $env:COMPUTERNAME
    $psi.LoadUserProfile = $true
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.WorkingDirectory = $WorkingDirectory
    # The environment is left alone, so the process gets the one of the
    # user's profile (not this runner's MINION_STATE_DIR, APPDATA, ...).
    $p = [System.Diagnostics.Process]::Start($psi)
    $p.WaitForExit()
    $code = $p.ExitCode
    Write-Host "[$($Credential.UserName)] $CommandLine -> exit $code"
    return $code
}

# Get-CiPipeProbeCommandLine returns a command line that connects to the named
# pipe \\.\pipe\$Name as whoever runs it and exits 0 when connected, 5 when
# access is denied, 2 when nothing answers in 5 seconds, 9 on another error.
# It is a PowerShell -EncodedCommand, so no quoting reaches cmd.exe.
function Get-CiPipeProbeCommandLine([string]$Name) {
    $probe = @"
`$c = [System.IO.Pipes.NamedPipeClientStream]::new('.', '$Name', [System.IO.Pipes.PipeDirection]::InOut)
`$code = 9
try { `$c.Connect(5000); 'connected'; `$code = 0 }
catch {
    `$e = `$_.Exception
    while (`$e) {
        if (`$e -is [System.UnauthorizedAccessException]) { `$code = 5; break }
        if (`$e -is [System.TimeoutException]) { `$code = 2; break }
        `$e = `$e.InnerException
    }
    "probe: `$(`$_.Exception.GetType().FullName): `$(`$_.Exception.Message)"
}
finally { `$c.Dispose() }
exit `$code
"@
    $enc = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($probe))
    $pwsh = (Get-Command pwsh).Source
    return "`"$pwsh`" -NoProfile -NonInteractive -EncodedCommand $enc"
}

# Assert-Ci throws $Message when $Condition is false.
function Assert-Ci([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw "assertion failed: $Message" }
}

# Show-CiFile prints a file of a principal's run (missing: says so).
function Show-CiFile([string]$Path) {
    if (Test-Path $Path) { Write-Host "--- $Path"; Get-Content -Raw $Path | Write-Host } else { Write-Host "--- $Path (missing)" }
}

# Get-CiServiceStatus returns the current state of the service (a fresh query).
function Get-CiServiceStatus([string]$Name = 'MinionWorker') {
    return [string](Get-Service -Name $Name).Status
}

# Get-CiServiceEnvironment returns the Environment value of the service
# (REG_MULTI_SZ, one NAME=value per entry).
function Get-CiServiceEnvironment([string]$Name = 'MinionWorker') {
    return [string[]](Get-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$Name" -Name Environment).Environment
}
