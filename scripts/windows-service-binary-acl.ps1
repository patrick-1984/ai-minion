# windows-service-binary-acl.ps1: who may replace the binary of a Windows
# service (security fix S1, .github/workflows/windows.yml job worker-service).
#
# It reads the executable from the service's ImagePath (or takes -Path), prints
# the owner and the ACL of the file and of every directory up to the volume
# root, and lists every allow entry that gives a non-administrator principal a
# right that could replace the binary: Users (S-1-5-32-545), Authenticated
# Users (S-1-5-11), INTERACTIVE (S-1-5-4), Everyone (S-1-1-0) and the user
# that runs this script. A non-administrator owner counts too (an owner can
# always change the DACL).
#
# -Expect safe     fails when anything is found, and when the binary is not
#                  below %ProgramFiles%\Minion;
# -Expect writable fails when nothing is found (the layout before the fix).
param(
    [string]$Service = 'MinionWorker',
    [string]$Path = '',
    [ValidateSet('safe', 'writable')][string]$Expect = 'safe'
)
$ErrorActionPreference = 'Stop'

if (-not $Path) {
    $img = (Get-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Services\$Service").ImagePath
    if ($img -match '^"([^"]+)"') { $Path = $Matches[1] } else { $Path = ($img -split ' ')[0] }
    Write-Host "ImagePath of ${Service}: $img"
}
Write-Host "binary: $Path"

$me = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$untrusted = @('S-1-5-32-545', 'S-1-5-11', 'S-1-5-4', 'S-1-1-0', $me)
$fileRights = [Security.AccessControl.FileSystemRights]'WriteData, AppendData, WriteExtendedAttributes, WriteAttributes, Delete, ChangePermissions, TakeOwnership'
$dirRights = $fileRights -bor [Security.AccessControl.FileSystemRights]'DeleteSubdirectoriesAndFiles'
$aboveRights = [Security.AccessControl.FileSystemRights]'DeleteSubdirectoriesAndFiles, Delete, ChangePermissions, TakeOwnership'
# Generic rights as they can appear in an ACE (GENERIC_ALL, GENERIC_WRITE).
$generic = 0x10000000 -bor 0x40000000

function SidOf($ref) {
    try { return $ref.Translate([Security.Principal.SecurityIdentifier]).Value } catch { return "$ref" }
}

$found = @()
$p = $Path
$role = 'file'
while ($p) {
    $acl = Get-Acl -LiteralPath $p
    $ownerSid = SidOf ([Security.Principal.NTAccount]$acl.Owner)
    Write-Host "--- $p ($role) owner $($acl.Owner) [$ownerSid]"
    $acl.Access | Format-Table IdentityReference, AccessControlType, FileSystemRights, InheritanceFlags, PropagationFlags, IsInherited -AutoSize | Out-String | Write-Host
    if ($untrusted -contains $ownerSid) { $found += "${p}: owned by $($acl.Owner)" }
    $mask = switch ($role) { 'file' { $fileRights } 'dir' { $dirRights } default { $aboveRights } }
    foreach ($r in $acl.Access) {
        if ($r.AccessControlType -ne 'Allow') { continue }
        if ($r.PropagationFlags -band [Security.AccessControl.PropagationFlags]::InheritOnly) { continue }
        $sid = SidOf $r.IdentityReference
        if ($untrusted -notcontains $sid) { continue }
        $rights = [int64]$r.FileSystemRights
        if ((($rights -band [int64]$mask) -ne 0) -or (($rights -band $generic) -ne 0)) {
            $found += "${p}: $($r.IdentityReference) has $($r.FileSystemRights)"
        }
    }
    $parent = Split-Path -Parent $p
    if (-not $parent -or $parent -eq $p) { break }
    $p = $parent
    $role = if ($role -eq 'file') { 'dir' } else { 'above' }
}

Write-Host "=== non-administrator write access: $($found.Count)"
$found | ForEach-Object { Write-Host "  $_" }
if ($Expect -eq 'safe') {
    $pf = [Environment]::GetFolderPath('ProgramFiles')
    if (-not $Path.StartsWith("$pf\Minion\", [StringComparison]::OrdinalIgnoreCase)) {
        throw "the service binary $Path is not below $pf\Minion"
    }
    if ($found.Count -gt 0) { throw "the service binary can be replaced by a non-administrator" }
} elseif ($found.Count -eq 0) {
    throw "expected the binary to be writable by this user, found nothing"
}
