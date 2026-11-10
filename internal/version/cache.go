package version

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Cache keeps verified releases on disk so that an offline "minion update"
// can install the newest verified version that matches a selector (AD-24).
// One directory per tag holds the release archive (whose digest the signed
// checksums vouch for), the unpacked binary, the signed checksums and manifest
// with their bundles, and meta.json. An entry is written to a staging
// directory and renamed into place only after every check passed, so a
// directory named like a tag is always complete.
type Cache struct {
	// Dir is the cache root. It is created on first use.
	Dir string
}

const metaFile = "meta.json"

// CacheMeta records what was verified when the release was cached.
type CacheMeta struct {
	Tag           string `json:"tag"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	ArchiveName   string `json:"archive_name"`
	ArchiveSHA256 string `json:"archive_sha256"`
	BinarySHA256  string `json:"binary_sha256"`
	// Prerelease is the release host's pre-release flag at caching time. An
	// offline channel selection honours it like the online one. meta.json is
	// not signed, so this only narrows what a channel may pick among
	// verified releases; it never authenticates anything.
	Prerelease bool `json:"prerelease,omitempty"`
}

// EntryDir returns the directory of the cache entry for a tag.
func (c Cache) EntryDir(tag string) string { return filepath.Join(c.Dir, tag) }

// Prepare creates the cache root (mode 0700) when it does not exist and
// refuses it unless only trusted users can change it: the root and every
// directory on the way to it must be owned by root (or an administrative
// account on Windows) or by the running user, with no write access for group,
// others or anyone else, and no untrusted link on the path (see
// checkTrustedDir). A cache another local user could write would let them swap
// a file after the updater verified it. Nothing is repaired or used from an
// unsafe cache: the error names it and says how to choose another.
func (c Cache) Prepare() error {
	if c.Dir == "" {
		return errors.New("no update cache directory configured")
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return fmt.Errorf("cannot create the update cache: %w", err)
	}
	return c.checkTrusted()
}

// checkTrusted runs the trust check on an existing cache root.
func (c Cache) checkTrusted() error {
	err := checkTrustedDir(c.Dir)
	if errors.Is(err, ErrUntrustedCache) {
		return fmt.Errorf("update cache %q: %w. %s", c.Dir, err, untrustedGuidance)
	}
	return err
}

// prepareExisting is the offline counterpart of [Cache.Prepare]: it never
// creates anything. A cache that does not exist is not an error (there is
// nothing to use); one that exists must be trusted.
func (c Cache) prepareExisting() (exists bool, err error) {
	if c.Dir == "" {
		return false, nil
	}
	if err := c.checkTrusted(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// NewStaging creates an empty staging directory inside the cache root, which
// it first checks with [Cache.Prepare].
func (c Cache) NewStaging() (string, error) {
	if err := c.Prepare(); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(c.Dir, ".stage-")
	if err != nil {
		return "", err
	}
	// The new directory inherited its access rights from the cache root. Check
	// what it got before anything authenticated is written into it.
	if err := c.checkNew(staging); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}
	return staging, nil
}

// checkNew runs the trust check on a directory that was just created below the
// cache root.
func (c Cache) checkNew(dir string) error {
	if err := checkTrustedDir(dir); err != nil {
		if errors.Is(err, ErrUntrustedCache) {
			return fmt.Errorf("new directory %q in the update cache: %w. %s", dir, err, untrustedGuidance)
		}
		return err
	}
	return nil
}

// Commit moves a complete staging directory into place as the entry for tag,
// replacing an older entry.
func (c Cache) Commit(staging, tag string) error {
	final := c.EntryDir(tag)
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	return os.Rename(staging, final)
}

// Tags returns the versions that have a cache entry, newest first. Staging
// directories and names that are not release tags are ignored.
func (c Cache) Tags() ([]SemVer, error) {
	ents, err := os.ReadDir(c.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []SemVer
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		v, err := ParseVersion(e.Name())
		if err != nil || v.String() != e.Name() {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) > 0 })
	return out, nil
}

// writeMeta stores meta.json in a staging directory.
func writeMeta(dir string, m CacheMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, metaFile), append(data, '\n'), 0o600)
}

// readTrustedFile reads at most limit bytes of the cache file path through
// [openTrusted]: one open that does not follow a link, checked on the handle.
func readTrustedFile(path string, limit int64) ([]byte, error) {
	f, err := openTrusted(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

func readMeta(dir string) (CacheMeta, error) {
	data, err := readTrustedFile(filepath.Join(dir, metaFile), maxManifestSize)
	if err != nil {
		return CacheMeta{}, err
	}
	var m CacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return CacheMeta{}, fmt.Errorf("corrupt %s: %w", metaFile, err)
	}
	return m, nil
}
