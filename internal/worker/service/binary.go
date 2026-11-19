package service

import (
	"fmt"
	"sort"
	"strings"
)

// The binary of the Windows service (security fix S1, after release 0.13.1). The service
// runs as LocalSystem, so whoever can change the file it starts, or a
// directory on the way to it, can run code as SYSTEM at the next start. The
// install therefore never registers the binary where the user put it (the
// installer uses %LOCALAPPDATA%\Programs\minion, which the user can write):
// it copies the running binary into %ProgramFiles%\Minion, a directory whose
// protected DACL lets only SYSTEM and the administrators write (users may read
// and run it), and registers that copy. The rest of this file is the pure
// logic of that, tested on every platform; the Windows API side lives in
// binary_windows.go.

// ServiceBinaryDirName is the directory below %ProgramFiles% that holds the
// binary of the Windows service.
const ServiceBinaryDirName = "Minion"

// serviceBinaryName is the file name of the service binary.
const serviceBinaryName = "minion.exe"

// ServiceBinaryDir returns %ProgramFiles%\Minion for the given Program Files
// directory. It joins with a backslash on every platform, so that it can be
// tested anywhere.
func ServiceBinaryDir(programFiles string) string {
	return strings.TrimRight(programFiles, `\/`) + `\` + ServiceBinaryDirName
}

// ServiceBinaryPath returns %ProgramFiles%\Minion\minion.exe.
func ServiceBinaryPath(programFiles string) string {
	return ServiceBinaryDir(programFiles) + `\` + serviceBinaryName
}

// SameWindowsPath reports whether a and b name the same path on Windows:
// case-insensitive, either slash, without a trailing separator. It does not
// resolve links or short (8.3) names.
func SameWindowsPath(a, b string) bool {
	norm := func(p string) string {
		p = strings.ReplaceAll(strings.TrimSpace(p), "/", `\`)
		p = strings.TrimPrefix(p, `\\?\`)
		if len(p) > 3 {
			p = strings.TrimRight(p, `\`)
		}
		return p
	}
	return a != "" && b != "" && strings.EqualFold(norm(a), norm(b))
}

// ParseServiceImagePath returns the executable of a service command line
// (the ImagePath of the service, BinaryPathName of QueryServiceConfig): the
// quoted first part, or, unquoted, everything up to and including the first
// ".exe". An unquoted path is also what Windows itself would parse
// ambiguously; the caller treats that as a finding of its own.
func ParseServiceImagePath(cmdline string) string {
	s := strings.TrimSpace(cmdline)
	if strings.HasPrefix(s, `"`) {
		if end := strings.Index(s[1:], `"`); end >= 0 {
			return s[1 : 1+end]
		}
		return strings.TrimPrefix(s, `"`)
	}
	if i := strings.Index(strings.ToLower(s), ".exe"); i >= 0 {
		return s[:i+len(".exe")]
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

// Windows access rights, by value so that this file builds everywhere (the
// windows package names most of them; FILE_DELETE_CHILD it does not).
const (
	rightWriteData      = 0x2 // FILE_WRITE_DATA, FILE_ADD_FILE on a directory
	rightAppendData     = 0x4 // FILE_APPEND_DATA, FILE_ADD_SUBDIRECTORY on a directory
	rightWriteEA        = 0x10
	rightDeleteChild    = 0x40
	rightWriteAttribute = 0x100
	rightDelete         = 0x10000
	rightWriteDAC       = 0x40000
	rightWriteOwner     = 0x80000
	rightMaximumAllowed = 0x2000000
	rightGenericAll     = 0x10000000
	rightGenericWrite   = 0x40000000
)

// PathRole is the place of a path on the way to the service binary; it decides
// which rights count as "can replace the binary".
type PathRole int

const (
	// RoleFile is the binary itself: changing its content, its DACL or its
	// owner, or deleting (renaming) it.
	RoleFile PathRole = iota
	// RoleDir is the directory that holds the binary: adding a file (a DLL
	// next to it, or a replacement renamed into place), deleting or renaming
	// an entry, or taking over the directory.
	RoleDir
	// RoleAncestor is every directory above: adding a sibling is harmless,
	// renaming or deleting the directory on the way, or taking it over, is not.
	RoleAncestor
)

func (r PathRole) mask() uint32 {
	switch r {
	case RoleFile:
		return rightWriteData | rightAppendData | rightWriteEA | rightWriteAttribute | rightDelete |
			rightWriteDAC | rightWriteOwner | rightGenericWrite | rightGenericAll | rightMaximumAllowed
	case RoleDir:
		return rightWriteData | rightAppendData | rightWriteEA | rightWriteAttribute | rightDeleteChild |
			rightDelete | rightWriteDAC | rightWriteOwner | rightGenericWrite | rightGenericAll | rightMaximumAllowed
	default:
		return rightDeleteChild | rightDelete | rightWriteDAC | rightWriteOwner | rightGenericAll | rightMaximumAllowed
	}
}

// ACE types this check evaluates.
const (
	aceAllowed = 0x0 // ACCESS_ALLOWED_ACE_TYPE
	aceDenied  = 0x1 // ACCESS_DENIED_ACE_TYPE
)

// ACE flags this check reads.
const (
	aceObjectInherit    = 0x1
	aceContainerInherit = 0x2
	aceInheritOnly      = 0x8
)

// Well-known SIDs.
const (
	sidSystem          = "S-1-5-18"
	sidAdministrators  = "S-1-5-32-544"
	sidTrustedInstall  = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	sidCreatorOwner    = "S-1-3-0"
	sidOwnerRights     = "S-1-3-4"
	sidEveryone        = "S-1-1-0"
	sidUsers           = "S-1-5-32-545"
	sidAuthenticated   = "S-1-5-11"
	sidInteractive     = "S-1-5-4"
	sidNullDACLLabel   = "Everyone (no DACL)"
	sidUnsupportedACE  = "an ACE this check cannot evaluate"
	sidNoOwnerSentinel = "no owner"
	sidReparseLabel    = "a symbolic link or junction"
)

// trustedWriter reports whether sid may write the service binary: SYSTEM, the
// administrators and TrustedInstaller, which can all run code as SYSTEM
// anyway. The installing user is deliberately not trusted, even when that
// user is an administrator: the user's unelevated processes run with that SID.
func trustedWriter(sid string) bool {
	switch strings.ToUpper(sid) {
	case sidSystem, sidAdministrators, strings.ToUpper(sidTrustedInstall):
		return true
	}
	return false
}

// ACE is one access control entry of a security descriptor, reduced to what
// the check reads.
type ACE struct {
	// Type is the ACE type (0 allowed, 1 denied; anything else is not
	// evaluated and counts as a write grant to someone unknown).
	Type uint8
	// Flags are the ACE flags (inheritance).
	Flags uint8
	// Mask is the access mask.
	Mask uint32
	// SID is the trustee, as a string SID.
	SID string
}

// SecurityInfo is the owner and the DACL of a file or directory.
type SecurityInfo struct {
	// Owner is the owner SID; empty when there is none.
	Owner string
	// NullDACL is true when the object has no DACL, which grants everyone
	// full access.
	NullDACL bool
	// DACL is the list of ACEs, in order.
	DACL []ACE
	// Reparse is true when the path is a symbolic link, junction or mount
	// point: whoever controls its target controls what the path names.
	Reparse bool
}

// UntrustedWriters returns the principals other than SYSTEM, the
// administrators and TrustedInstaller that could replace the service binary
// through the object sd describes, in the given role, sorted and without
// duplicates. It is strict where it cannot be exact: deny ACEs are ignored
// (they would need group membership to evaluate), and an ACE type other than
// plain allow or deny counts as a grant to someone unknown.
//
//   - An owner that is not trusted can always change the DACL, unless an
//     OWNER RIGHTS ACE limits the owner's implicit rights.
//   - An ACE that only children inherit (inherit-only) does not apply to the
//     object itself. On the directory of the binary it still counts when files
//     or subdirectories inherit it: every file created there later (a
//     self-update's new binary, for one) would get it. CREATOR OWNER in such an
//     ACE becomes the creator of the new file, which only an administrator or
//     SYSTEM can be in a directory nobody else may write.
func UntrustedWriters(sd SecurityInfo, role PathRole) []string {
	set := map[string]bool{}
	if sd.Reparse {
		set[sidReparseLabel] = true
	}
	if sd.NullDACL {
		set[sidNullDACLLabel] = true
	}
	ownerRightsACE := false
	for _, a := range sd.DACL {
		if strings.EqualFold(a.SID, sidOwnerRights) && a.Flags&aceInheritOnly == 0 {
			ownerRightsACE = true
		}
	}
	owner := sd.Owner
	if owner == "" {
		owner = sidNoOwnerSentinel
	}
	if !trustedWriter(owner) && !ownerRightsACE {
		set[owner] = true
	}
	mask := role.mask()
	for _, a := range sd.DACL {
		inheritOnly := a.Flags&aceInheritOnly != 0
		inherits := a.Flags&(aceObjectInherit|aceContainerInherit) != 0
		if inheritOnly && (role != RoleDir || !inherits) {
			continue
		}
		switch a.Type {
		case aceDenied:
			continue
		case aceAllowed:
		default:
			set[sidUnsupportedACE] = true
			continue
		}
		m := mask
		if inheritOnly && a.Flags&aceContainerInherit == 0 {
			m = RoleFile.mask() // only files inherit it
		}
		if a.Mask&m == 0 {
			continue
		}
		sid := a.SID
		switch {
		case strings.EqualFold(sid, sidCreatorOwner):
			// Inherited, it becomes the creator of the new object; on the
			// object itself it grants nothing.
			continue
		case strings.EqualFold(sid, sidOwnerRights):
			sid = owner
		}
		if trustedWriter(sid) {
			continue
		}
		set[sid] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ChainStep is one path on the way to the service binary and its role.
type ChainStep struct {
	Path string
	Role PathRole
}

// BinaryChain returns the binary, its directory and every directory above it
// up to the volume root, for a Windows path ("C:\a\b.exe" or
// "\\server\share\a\b.exe"). Either slash is accepted.
func BinaryChain(binary string) []ChainStep {
	p := strings.ReplaceAll(binary, "/", `\`)
	p = strings.TrimPrefix(p, `\\?\`)
	var root, rest string
	switch {
	case strings.HasPrefix(p, `\\`):
		parts := strings.SplitN(strings.TrimPrefix(p, `\\`), `\`, 3)
		if len(parts) < 2 {
			return []ChainStep{{Path: binary, Role: RoleFile}}
		}
		root = `\\` + parts[0] + `\` + parts[1] + `\`
		if len(parts) == 3 {
			rest = parts[2]
		}
	case len(p) >= 2 && p[1] == ':':
		root = p[:2] + `\`
		rest = strings.TrimLeft(p[2:], `\`)
	default:
		return []ChainStep{{Path: binary, Role: RoleFile}}
	}
	var comps []string
	for _, c := range strings.Split(rest, `\`) {
		if c != "" {
			comps = append(comps, c)
		}
	}
	if len(comps) == 0 {
		return []ChainStep{{Path: root, Role: RoleFile}}
	}
	steps := make([]ChainStep, 0, len(comps)+1)
	for i := len(comps); i >= 0; i-- {
		path := root + strings.Join(comps[:i], `\`)
		role := RoleAncestor
		switch i {
		case len(comps):
			role = RoleFile
		case len(comps) - 1:
			role = RoleDir
		}
		steps = append(steps, ChainStep{Path: path, Role: role})
	}
	return steps
}

// BinaryFinding is one principal that can replace the service binary through
// one path.
type BinaryFinding struct {
	// Path is the file or directory whose owner or DACL gives the access.
	Path string
	// SID is the principal; Name its account name when it could be looked up.
	SID  string
	Name string
}

func (f BinaryFinding) who() string {
	if f.Name != "" {
		return f.Name
	}
	switch strings.ToUpper(f.SID) {
	case sidUsers:
		return `BUILTIN\Users`
	case sidAuthenticated:
		return `NT AUTHORITY\Authenticated Users`
	case sidInteractive:
		return `NT AUTHORITY\INTERACTIVE`
	case sidEveryone:
		return "Everyone"
	}
	return f.SID
}

// FindingsFor runs [UntrustedWriters] over the chain of binary with the
// security info that read returns for each path.
func FindingsFor(binary string, read func(path string) (SecurityInfo, error)) ([]BinaryFinding, error) {
	var out []BinaryFinding
	for _, s := range BinaryChain(binary) {
		sd, err := read(s.Path)
		if err != nil {
			return out, fmt.Errorf("cannot read the security of %s: %w", s.Path, err)
		}
		for _, sid := range UntrustedWriters(sd, s.Role) {
			out = append(out, BinaryFinding{Path: s.Path, SID: sid})
		}
	}
	return out, nil
}

// ServiceBinaryWarning is the problem line of "minion worker status" for a
// service binary that principals other than SYSTEM and the administrators can
// replace; empty when findings is empty.
func ServiceBinaryWarning(binary string, findings []BinaryFinding) string {
	if len(findings) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var who []string
	for _, f := range findings {
		w := f.who() + " (" + f.Path + ")"
		if !seen[w] {
			seen[w] = true
			who = append(who, w)
		}
	}
	const maxShown = 4
	more := ""
	if len(who) > maxShown {
		more = fmt.Sprintf(" and %d more", len(who)-maxShown)
		who = who[:maxShown]
	}
	return fmt.Sprintf("the service binary %s of the Windows service %s is writable by non-administrators: %s%s; "+
		"any process of those accounts could run code as LocalSystem at the next service start. "+
		"Do not run that file elevated. Move the service to the verified release in %%ProgramFiles%%\\%s: in an elevated PowerShell run %s",
		binary, WindowsServiceName, strings.Join(who, ", "), more, ServiceBinaryDirName, BootstrapCommand())
}
