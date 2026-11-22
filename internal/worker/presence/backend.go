package presence

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// backend is the file side of one layout.
type backend struct{ l Layout }

func (b backend) public() bool { return b.l.Scope == ScopeRoot }

// prepare creates and verifies the directories (and the discovery entry
// directory of a relocated user profile).
func (b backend) prepare(now time.Time) error {
	if err := b.l.validate(); err != nil {
		return err
	}
	if b.public() {
		_, stage, err := ensurePublicDirs(b.l)
		if err != nil {
			return err
		}
		sweepTemps(stage, now)
		return nil
	}
	dir, err := ensurePrivateDir(b.l.PrivateDir, b.l.UID)
	if err != nil {
		return err
	}
	sweepTemps(dir, now)
	return nil
}

func (b backend) write(name string, data []byte) error {
	if b.public() {
		return writePublic(b.l, name, data)
	}
	return writePrivate(b.l.PrivateDir, name, data, b.l.UID)
}

func (b backend) read(name string, max int64) ([]byte, error) {
	if b.public() {
		root, err := verifyPublicDir(b.l.PublicRoot, b.l.RootUID)
		if err != nil {
			return nil, err
		}
		return readPublicFile(root, name, b.l.RootUID, max)
	}
	dir, err := verifyPrivateDir(b.l.PrivateDir, b.l.UID)
	if err != nil {
		return nil, err
	}
	return readPrivateFile(dir, name, b.l.UID, max)
}

func (b backend) physDir() (string, error) {
	if b.public() {
		return verifyPublicDir(b.l.PublicRoot, b.l.RootUID)
	}
	return verifyPrivateDir(b.l.PrivateDir, b.l.UID)
}

func (b backend) remove(name string) error {
	dir, err := b.physDir()
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

func (b backend) exists(name string) bool {
	dir, err := b.physDir()
	if err != nil {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, name))
	return err == nil
}

// instances lists the instance ids that have a record or usage file.
func (b backend) instances() []string {
	dir, err := b.physDir()
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range ents {
		id, _, ok := splitName(e.Name())
		if ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// splitName parses <instance>.json and <instance>.usage.json.
func splitName(n string) (id string, usage, ok bool) {
	switch {
	case strings.HasSuffix(n, usageSuffix):
		id, usage = strings.TrimSuffix(n, usageSuffix), true
	case strings.HasSuffix(n, recordSuffix):
		id = strings.TrimSuffix(n, recordSuffix)
	default:
		return "", false, false
	}
	return id, usage, ValidInstance(id)
}

// claimPath is the root-only note of the instance that publishes for this
// profile (public layout only; a user directory is owned by one daemon).
func (b backend) claimPath() (string, bool) {
	if !b.public() {
		return "", false
	}
	root, err := verifyPublicDir(b.l.PublicRoot, b.l.RootUID)
	if err != nil {
		return "", false
	}
	return filepath.Join(root, stageDirName, "profile-"+b.l.profileKey()), true
}

func (b backend) claimed() string {
	p, ok := b.claimPath()
	if !ok {
		return ""
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(raw))
	if !ValidInstance(id) {
		return ""
	}
	return id
}

func (b backend) setClaim(instance string) error {
	p, ok := b.claimPath()
	if !ok {
		return nil
	}
	tmp := p + ".new"
	if err := os.WriteFile(tmp, []byte(instance), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (b backend) dropClaim() {
	if p, ok := b.claimPath(); ok {
		_ = os.Remove(p)
	}
}

// current reports whether instance still owns this profile's publication: it
// was neither cleared by a reset nor replaced by a successor.
func (b backend) current(instance string) bool {
	if b.public() {
		return b.claimed() == instance
	}
	return b.exists(instance + recordSuffix)
}

// predecessors returns the instances of this profile other than self whose
// files must go when self starts.
func (b backend) predecessors(self string) []string {
	var out []string
	if b.public() {
		if c := b.claimed(); c != "" && c != self {
			out = append(out, c)
		}
		return out
	}
	for _, id := range b.instances() {
		if id != self {
			out = append(out, id)
		}
	}
	return out
}

func (b backend) removeInstance(id string) error {
	e1 := b.remove(id + recordSuffix)
	e2 := b.remove(id + usageSuffix)
	if e1 != nil {
		return e1
	}
	return e2
}
