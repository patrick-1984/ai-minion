package filexfer

import (
	"os"
	"path/filepath"
	"strings"
)

// Identity is the identity of a file system object: device and inode
// (st_dev, st_ino). Two handles with the same Identity name the same object,
// whatever path reached it; a bind mount keeps the identity of its source.
type Identity struct {
	Dev uint64
	Ino uint64
}

// IsZero reports whether id is the zero Identity (unknown).
func (id Identity) IsZero() bool { return id == Identity{} }

// IdentitySet is a set of identities.
type IdentitySet map[Identity]struct{}

// Add adds id; the zero Identity is never added.
func (s IdentitySet) Add(id Identity) {
	if !id.IsZero() {
		s[id] = struct{}{}
	}
}

// Has reports whether id is in the set.
func (s IdentitySet) Has(id Identity) bool {
	_, ok := s[id]
	return ok
}

// FloorConfig names the folders of the Floor. Empty members are skipped;
// folders that do not exist are skipped (their names still match by
// SensitiveDirPath and ProtectedDirPath).
type FloorConfig struct {
	// Home is the Worker user's home directory.
	Home string
	// StateDir is the Worker (or CLI) state directory: sensitive and
	// push-protected, and it and each of its ancestors are refused as a root.
	StateDir string
	// AllowHome allows home itself as a root (file_transfer_allow_home).
	AllowHome bool
	// ProtectedOff relaxes the push-protected names and folders
	// (file_transfer_protected=off). The floor and the sensitive folders
	// stay.
	ProtectedOff bool
	// System replaces the default list of system folders that are refused as
	// a root (nil: "/", "/usr", "/var", ... per OS); SystemDeep the default
	// list of system folders refused as a root at and below them (nil:
	// "/proc", "/sys", "/dev", "/etc", "/boot", "/run"; macOS "/System",
	// "/private/etc", "/dev", "/cores"). Tests set them.
	System     []string
	SystemDeep []string
}

// Floor is the identity floor of one end, recorded at start (and again when the
// configuration changes): the folders a root may not be, the folders a root
// may not be inside, the sensitive folders (never sent) and the push-protected
// folders (never written), all by identity (BRD FR-3).
type Floor struct {
	equal        IdentitySet // refused as a root
	deep         IdentitySet // refused as a root and as an ancestor of a root
	sensitive    IdentitySet // refused at and below; skipped or refused in walks
	protected    IdentitySet // push-protected at and below
	home         Identity
	allowHome    bool
	protectedOff bool
}

// sensitiveUnderHome are the sensitive folders below home recorded by identity.
var sensitiveUnderHome = []string{".ssh", ".gnupg", ".aws", ".config/minion"}

// NewFloor records the identities of the floor folders. A folder that cannot
// be read is skipped: the floor is a refusal list, so a missing entry only
// matters when the folder exists and could become a root, and then the root
// opener can read it.
func NewFloor(cfg FloorConfig) *Floor {
	f := &Floor{
		equal: IdentitySet{}, deep: IdentitySet{}, sensitive: IdentitySet{}, protected: IdentitySet{},
		allowHome: cfg.AllowHome, protectedOff: cfg.ProtectedOff,
	}
	system, deep := cfg.System, cfg.SystemDeep
	if system == nil {
		system = defaultSystemFolders
	}
	if deep == nil {
		deep = defaultSystemDeepFolders
	}
	for _, p := range system {
		f.equal.Add(statIdentity(p))
	}
	for _, p := range deep {
		f.deep.Add(statIdentity(p))
	}
	if cfg.Home != "" {
		f.home = statIdentity(cfg.Home)
		addAncestors(f.equal, cfg.Home)
		for _, s := range sensitiveUnderHome {
			id := statIdentity(filepath.Join(cfg.Home, filepath.FromSlash(s)))
			f.sensitive.Add(id)
			if s == ".ssh" || s == ".gnupg" {
				f.protected.Add(id)
			}
		}
	}
	if cfg.StateDir != "" {
		id := statIdentity(cfg.StateDir)
		f.sensitive.Add(id)
		f.protected.Add(id)
		addAncestors(f.equal, cfg.StateDir)
	}
	return f
}

// addAncestors adds the identities of the strict ancestors of p.
func addAncestors(s IdentitySet, p string) {
	p = filepath.Clean(p)
	for {
		parent := filepath.Dir(p)
		if parent == p {
			return
		}
		s.Add(statIdentity(parent))
		p = parent
	}
}

// statIdentity returns the identity of the directory p, following symlinks
// (the floor records the objects, and a root is never a symlink), or the zero
// Identity when p cannot be read.
func statIdentity(p string) Identity {
	fi, err := os.Stat(p)
	if err != nil {
		return Identity{}
	}
	id, _ := identityOfInfo(fi)
	return id
}

// SensitiveDir reports whether id is a sensitive folder (.ssh, .gnupg, .aws,
// .config/minion under home, the state directory) or a system folder refused at
// and below: a folder pull skips it with proto.SkipSensitive and a direct pull
// refuses it with path_sensitive, whatever its name.
func (f *Floor) SensitiveDir(id Identity) bool {
	return f != nil && (f.sensitive.Has(id) || f.deep.Has(id))
}

// ProtectedDir reports whether a push may not write into the folder id (the
// state directory, ~/.ssh, ~/.gnupg, sensitive and deep system folders). It is
// false for the push-protected entries when ProtectedOff is set, but never for
// sensitive and system folders.
func (f *Floor) ProtectedDir(id Identity) bool {
	if f == nil {
		return false
	}
	if f.sensitive.Has(id) || f.deep.Has(id) {
		return true
	}
	return !f.protectedOff && f.protected.Has(id)
}

// ProtectedOff reports whether the owner relaxed the push-protected lists.
func (f *Floor) ProtectedOff() bool { return f != nil && f.protectedOff }

// checkRoot decides on a root with identity id whose ancestors (nearest first,
// root excluded) have the identities anc and whose path elements from "/" are
// names (an unknown name is ""). It returns a files_root_refused refusal, or
// whether the root is push-protected.
func (f *Floor) checkRoot(id Identity, anc []Identity, names []string) (pushProtected bool, err error) {
	if f != nil {
		isHome := !f.home.IsZero() && id == f.home
		if isHome && !f.allowHome {
			return false, rootRefused("the folder of the session is the home folder; change to a project folder first")
		}
		// Home is also an ancestor of the state directory; allow_home
		// lifts that equality, nothing else.
		if (f.equal.Has(id) && !isHome) || f.deep.Has(id) {
			return false, rootRefused("the folder of the session is a system folder; file transfer is refused there")
		}
		if f.sensitive.Has(id) {
			return false, rootRefused("the folder of the session is a sensitive folder; file transfer is refused there")
		}
		for _, a := range anc {
			if f.deep.Has(a) {
				return false, rootRefused("the folder of the session is inside a system folder; file transfer is refused there")
			}
			if f.sensitive.Has(a) {
				return false, rootRefused("the folder of the session is inside a sensitive folder; file transfer is refused there")
			}
			if !f.protectedOff && f.protected.Has(a) {
				pushProtected = true
			}
		}
		if !f.protectedOff && f.protected.Has(id) {
			pushProtected = true
		}
	}
	if SensitiveDirPath(names) {
		return false, rootRefused("the folder of the session is inside a sensitive folder; file transfer is refused there")
	}
	if !f.ProtectedOff() && ProtectedDirPath(names) {
		pushProtected = true
	}
	return pushProtected, nil
}

// Name lists of BRD FR-8. Elements compare case-insensitively (APFS and other
// case-insensitive volumes fold names), so ".GIT/Hooks" is protected too.
var (
	// protectedDirs are push-protected folders: a path at or below one of
	// these element sequences is protected.
	protectedDirs = [][]string{
		{".git", "hooks"}, {".hg"}, {".svn"}, {".ssh"}, {".gnupg"},
		{".claude"}, {".codex"}, {".cursor"}, {".github", "workflows"},
	}
	// protectedFiles are push-protected files, matched as element sequences.
	protectedFiles = [][]string{
		{".git", "config"}, {".vscode", "tasks.json"}, {".vscode", "settings.json"},
		{".mcp.json"}, {".npmrc"}, {".pypirc"}, {".netrc"}, {".gitlab-ci.yml"},
		{".bashrc"}, {".bash_profile"}, {".profile"}, {".zshrc"}, {".zprofile"},
		{".zshenv"}, {"config.fish"},
	}
	// sensitiveDirs are pull-sensitive folders.
	sensitiveDirs = [][]string{{".ssh"}, {".gnupg"}, {".aws"}, {".config", "minion"}}
	// sensitiveFiles are pull-sensitive files, matched as element sequences.
	sensitiveFiles = [][]string{{".netrc"}, {".git-credentials"}}
)

// hasSeq reports whether seq occurs as consecutive elements of elems.
func hasSeq(elems, seq []string) bool {
	for i := 0; i+len(seq) <= len(elems); i++ {
		ok := true
		for j, s := range seq {
			if !strings.EqualFold(elems[i+j], s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func hasAnySeq(elems []string, seqs [][]string) bool {
	for _, s := range seqs {
		if hasSeq(elems, s) {
			return true
		}
	}
	return false
}

// ProtectedDirPath reports whether the folder with the path elements elems is
// at or below a push-protected folder (".git/hooks", ".claude", ...). It is
// the rule for the ancestry of a root; file names do not count there.
func ProtectedDirPath(elems []string) bool { return hasAnySeq(elems, protectedDirs) }

// SensitiveDirPath reports whether the folder with the path elements elems is
// at or below a sensitive folder (".ssh", ".gnupg", ".aws", ".config/minion").
func SensitiveDirPath(elems []string) bool { return hasAnySeq(elems, sensitiveDirs) }

// ProtectedPath reports whether a push may not write the entry with the path
// elements elems (relative to the root): it is in or below a protected folder,
// is a protected file, or an element is ".env*". The Worker owner's
// ProtectedOff relaxes it (Floor.ProtectedOff); the caller checks that.
func ProtectedPath(elems []string) bool {
	if hasAnySeq(elems, protectedDirs) || hasAnySeq(elems, protectedFiles) {
		return true
	}
	for _, e := range elems {
		if len(e) >= 4 && strings.EqualFold(e[:4], ".env") {
			return true
		}
	}
	return false
}

// SensitivePath reports whether a pull may not send the entry with the path
// elements elems: it is in or below a sensitive folder, is ".netrc" or
// ".git-credentials", or its last element is "id_*" without ".pub", "*.pem" or
// "*.key".
func SensitivePath(elems []string) bool {
	if hasAnySeq(elems, sensitiveDirs) || hasAnySeq(elems, sensitiveFiles) {
		return true
	}
	if len(elems) == 0 {
		return false
	}
	last := strings.ToLower(elems[len(elems)-1])
	if strings.HasPrefix(last, "id_") && !strings.HasSuffix(last, ".pub") {
		return true
	}
	return strings.HasSuffix(last, ".pem") || strings.HasSuffix(last, ".key")
}
