package filexfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Staging and publication of received files (BRD FR-7, FR-9).
//
// A Stager writes every received file to a private staging name
// proto.StagingPrefix + "<transfer_id>-<n>" (O_CREAT|O_EXCL|O_NOFOLLOW, mode
// 0600) in one staging directory: the deepest directory of the destination
// that exists when the transfer starts, opened through the root. It hashes
// while writing, sets mode (0644 minus umask, never an exec bit) and mtime on
// the handle, fsyncs, and publishes nothing until Verify accepted the whole
// transfer. Publish then creates missing directories one component at a time
// (mkdirat, reopened through the root) and moves each file to its final name
// in manifest order without replacing anything (renameat2 RENAME_NOREPLACE or
// link+unlink on Linux and Android, renameatx_np RENAME_EXCL on macOS), or,
// with Overwrite, replacing only a regular file with one link. Abort removes
// every staged file that was not published.
//
// Use (receiver, folder):
//
//	st, err := NewStager(root, transferID, dest, ops.FileKindDir, opts)
//	defer st.Abort()
//	m, err := Unpack(decoded, st)         // or ReceiveFile for a single file
//	// the body ended: the trailers are available
//	err = st.Verify(m, manifestDigestFromTrailer)
//	// re-check the local switch, then
//	res, err := st.Publish()

// StageOptions control a Stager.
type StageOptions struct {
	Limits Limits
	// Overwrite replaces an existing regular file with one link; without it
	// an existing name is conflict.
	Overwrite bool
	// Check is called with the path (relative to the root) of every entry
	// before it is staged and returns an error to refuse the transfer (the
	// Worker: path_protected for a push-protected name or directory
	// identity). It may be nil.
	Check func(rootRel string, isDir bool) error
	// MinFreeBytes and MinFreePermille are the reserve the file system must
	// keep after each staged file (NFR-6); 0 means the defaults
	// (proto.MinFreeBytes, proto.MinFreePermille), a negative MinFreeBytes
	// disables the check.
	MinFreeBytes    int64
	MinFreePermille int64
}

// stagedEntry is one received entry.
type stagedEntry struct {
	entry proto.ManifestEntry // Path relative to the destination
	mtime time.Time
	tmp   string // staging name of a file
	done  bool   // published (or removed)
}

// Stager stages and publishes the received entries of one transfer.
type Stager struct {
	root       Root
	transferID string
	dest       string
	kind       string
	opts       StageOptions

	stageDir *os.File
	stageRel string
	stageID  [2]uint64

	mu       sync.Mutex
	entries  []stagedEntry
	seen     map[string]string // path -> type
	n        int
	bytes    int64
	verified bool
	closed   bool
}

// NewStager prepares the staging of a transfer of kind (ops.FileKindFile or
// ops.FileKindDir) to dest, a wire path relative to root: the file itself, or
// the folder that receives the archive's entries. A destination that is an
// existing non-directory (kind dir) or whose parent chain contains one is
// conflict.
func NewStager(root Root, transferID, dest, kind string, opts StageOptions) (*Stager, error) {
	if !sysSupported {
		return nil, errUnsupported
	}
	if !ops.ValidUUIDv7(transferID) {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the transfer id must be a UUIDv7")
	}
	if kind != ops.FileKindFile && kind != ops.FileKindDir {
		return nil, proto.NewError(proto.CodeInvalidArgument, "the kind must be file or dir")
	}
	if err := CheckPath(dest, kind == ops.FileKindDir); err != nil {
		return nil, err
	}
	opts.Limits = opts.Limits.Normalize()
	if err := opts.Limits.checkPathLimits(dest); err != nil {
		return nil, err
	}
	s := &Stager{root: root, transferID: transferID, dest: dest, kind: kind, opts: opts, seen: map[string]string{}}
	// The staging directory: the deepest existing directory of the chain.
	chain := dest
	if kind == ops.FileKindFile {
		chain, _ = splitParent(dest)
	}
	dir, err := root.OpenDir("")
	if err != nil {
		return nil, openErr(err, true)
	}
	rel := ""
	if chain != "" {
		for _, name := range strings.Split(chain, "/") {
			next := JoinPath(rel, name)
			si, err := statAt(dir, name)
			if err != nil {
				if isNotExist(err) {
					break
				}
				_ = dir.Close()
				return nil, internal("the destination cannot be read")
			}
			if si.Type != typeDir {
				_ = dir.Close()
				return nil, conflict("the destination exists and is not a folder")
			}
			sub, err := root.OpenDir(next)
			if err != nil {
				_ = dir.Close()
				return nil, openErr(err, true)
			}
			_ = dir.Close()
			dir, rel = sub, next
		}
	}
	st, err := fstatFile(dir)
	if err != nil {
		_ = dir.Close()
		return nil, internal("the destination cannot be read")
	}
	s.stageDir, s.stageRel, s.stageID = dir, rel, [2]uint64{st.Dev, st.Ino}
	return s, nil
}

// StagingDir returns the staging directory relative to the root and its
// identity (device, inode), for the staging log (cleanup.go).
func (s *Stager) StagingDir() (rel string, dev, ino uint64) {
	return s.stageRel, s.stageID[0], s.stageID[1]
}

// full returns the path relative to the root of an entry path.
func (s *Stager) full(rel string) string { return JoinPath(s.dest, rel) }

// admit checks a new entry: path, duplicates and order (its parent was
// received as a directory before), limits and the Check hook.
func (s *Stager) admit(rel, typ string, size int64) error {
	if s.closed {
		return internal("the transfer was ended")
	}
	if s.verified {
		return internal("the transfer was verified already")
	}
	if s.kind == ops.FileKindFile {
		if rel != "" || typ != proto.FileTypeFile || len(s.entries) > 0 {
			return integrity("a single-file transfer has exactly one file")
		}
	} else {
		if CheckPath(rel, false) != nil {
			return proto.NewError(proto.CodePathOutsideRoot, "the archive names an invalid path")
		}
		if _, dup := s.seen[rel]; dup {
			return integrity("the archive names a path twice")
		}
		if parent, _ := splitParent(rel); parent != "" && s.seen[parent] != proto.FileTypeDir {
			return integrity("the archive names an entry before its folder")
		}
	}
	full := s.full(rel)
	if err := CheckPath(full, false); err != nil {
		return err
	}
	if err := s.opts.Limits.checkPathLimits(full); err != nil {
		return err
	}
	if int64(len(s.entries))+1 > s.opts.Limits.MaxEntries {
		return tooLarge("the transfer has more entries than the limit")
	}
	if size < 0 || s.bytes+size > s.opts.Limits.MaxBytes {
		return tooLarge("the transfer has more bytes than the limit")
	}
	if s.opts.Check != nil {
		if err := s.opts.Check(full, typ == proto.FileTypeDir); err != nil {
			return err
		}
	}
	return nil
}

// StageDir records a received directory (rel relative to the destination).
// Directories are created only by Publish.
func (s *Stager) StageDir(rel string, mtime time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kind != ops.FileKindDir {
		return integrity("a single-file transfer has no folders")
	}
	if err := s.admit(rel, proto.FileTypeDir, 0); err != nil {
		return err
	}
	s.seen[rel] = proto.FileTypeDir
	s.entries = append(s.entries, stagedEntry{entry: proto.ManifestEntry{Path: rel, Type: proto.FileTypeDir}, mtime: mtime})
	return nil
}

// stageWriteHook, when set (tests), is called before each write of staged
// content and may fail it (disk-full injection).
var stageWriteHook func() error

// StageFile writes a received file (rel relative to the destination; "" for
// a single-file transfer) of exactly size bytes read from r to a new staging
// file and returns its SHA-256. Fewer bytes is integrity_failed; bytes beyond
// size are not read. A full file system is internal ("no space left") or,
// when the reserve would be used, transfer_too_large.
func (s *Stager) StageFile(rel string, size int64, mtime time.Time, r io.Reader) ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.admit(rel, proto.FileTypeFile, size); err != nil {
		return [32]byte{}, err
	}
	if err := s.checkFree(size); err != nil {
		return [32]byte{}, err
	}
	s.n++
	tmp := proto.StagingPrefix + s.transferID + "-" + strconv.Itoa(s.n)
	var f *os.File
	err := withFd(s.stageDir, func(fd int) error {
		var err error
		f, err = sysCreateExcl(fd, tmp)
		return err
	})
	if err != nil {
		if isNoSpace(err) {
			return [32]byte{}, internal("no space left on the receiving file system")
		}
		return [32]byte{}, internal("a staging file cannot be created")
	}
	// From here the staging file exists: record it so that Abort removes it.
	s.entries = append(s.entries, stagedEntry{entry: proto.ManifestEntry{Path: rel, Size: size, Type: proto.FileTypeFile}, mtime: mtime, tmp: tmp})
	idx := len(s.entries) - 1
	s.seen[rel] = proto.FileTypeFile
	sum, err := writeStaged(f, r, size, mtime)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = internal("a staging file cannot be written")
	}
	if err != nil {
		return [32]byte{}, err
	}
	s.entries[idx].entry.SHA256 = hex.EncodeToString(sum[:])
	s.bytes += size
	return sum, nil
}

type hookWriter struct{ w io.Writer }

func (h hookWriter) Write(p []byte) (int, error) {
	if err := stageWriteHook(); err != nil {
		return 0, err
	}
	return h.w.Write(p)
}

var umaskOnce = sync.OnceValue(sysUmask)

func writeStaged(f *os.File, r io.Reader, size int64, mtime time.Time) ([32]byte, error) {
	h := sha256.New()
	var dst io.Writer = f
	if stageWriteHook != nil {
		dst = hookWriter{f}
	}
	buf := make([]byte, copyBufSize)
	n, err := io.CopyBuffer(io.MultiWriter(dst, h), io.LimitReader(r, size), buf)
	if err != nil {
		var pe *proto.Error
		switch {
		case isNoSpace(err):
			return [32]byte{}, internal("no space left on the receiving file system")
		case errors.As(err, &pe):
			return [32]byte{}, pe
		}
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return [32]byte{}, internal("a staging file cannot be written")
		}
		return [32]byte{}, err
	}
	if n != size {
		return [32]byte{}, integrity("the content is shorter than declared")
	}
	mode := os.FileMode(0o644 &^ umaskOnce() & 0o666)
	if err := f.Chmod(mode); err != nil {
		return [32]byte{}, internal("a staging file cannot be written")
	}
	if err := withFd(f, func(fd int) error { return sysSetMTime(fd, paxTime(mtime)) }); err != nil {
		return [32]byte{}, internal("a staging file cannot be written")
	}
	if err := f.Sync(); err != nil {
		if isNoSpace(err) {
			return [32]byte{}, internal("no space left on the receiving file system")
		}
		return [32]byte{}, internal("a staging file cannot be written")
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum, nil
}

// checkFree refuses a file that would leave less than the reserve free.
func (s *Stager) checkFree(size int64) error {
	if s.opts.MinFreeBytes < 0 {
		return nil
	}
	minBytes, permille := s.opts.MinFreeBytes, s.opts.MinFreePermille
	if minBytes == 0 {
		minBytes = proto.MinFreeBytes
	}
	if permille <= 0 {
		permille = proto.MinFreePermille
	}
	var avail, total uint64
	err := withFd(s.stageDir, func(fd int) error {
		var err error
		avail, total, err = sysFreeBytes(fd)
		return err
	})
	if err != nil {
		return nil // unknown: the write itself fails on a full file system
	}
	reserve := max(uint64(minBytes), total/1000*uint64(permille))
	if uint64(size)+reserve > avail {
		return tooLarge("the receiving file system does not have enough free space")
	}
	return nil
}

// Received returns the received entries in order (paths relative to the
// destination), as a manifest.
func (s *Stager) Received() []proto.ManifestEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.ManifestEntry, len(s.entries))
	for i, e := range s.entries {
		out[i] = e.entry
	}
	return out
}

// Verify accepts the transfer for publication (FR-9): the received entries
// equal the manifest's in order (path, type, size, SHA-256), and the SHA-256
// of the manifest's canonical bytes equals digest (from the
// Minion-Manifest-Digest trailer). For a single file the manifest's one entry
// is compared by size, digest and type (its path names the source).
// Everything else (end of archive, clean end of the coding, trailers
// present, the Hub's commit gate) is the caller's to establish before
// calling Verify. A failure is integrity_failed and makes Publish impossible.
func (s *Stager) Verify(m Manifest, digest []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return internal("the transfer was ended")
	}
	got := make([]proto.ManifestEntry, len(s.entries))
	for i, e := range s.entries {
		got[i] = e.entry
	}
	want := m.Entries
	if s.kind == ops.FileKindFile && len(want) == 1 && len(got) == 1 {
		w := want[0]
		w.Path = ""
		want = []proto.ManifestEntry{w}
	}
	if err := sameEntries(got, want); err != nil {
		return err
	}
	sum, err := m.Digest()
	if err != nil || len(digest) != len(sum) || string(sum[:]) != string(digest) {
		return integrity("the manifest digest does not match")
	}
	s.verified = true
	return nil
}

// PublishResult is the outcome of Publish.
type PublishResult struct {
	// Published are the published files and created directories, paths
	// relative to the root, in manifest order.
	Published []ops.FileTransferEntry
	Files     int64
	Bytes     int64
}

// beforeRenameHook, when set (tests), runs right before a file is moved to
// its final name (dirfd of the destination directory, final name).
var beforeRenameHook func(dir *os.File, name string)

// Publish makes the verified transfer visible in manifest order. It first
// checks every destination name (no file published when one conflicts),
// then creates directories and moves files. A failure after something was
// published returns the partial result with the error; the remaining staged
// files are removed.
func (s *Stager) Publish() (PublishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return PublishResult{}, internal("the transfer was ended")
	}
	if !s.verified {
		return PublishResult{}, integrity("the transfer was not verified")
	}
	defer func() { _ = s.abortLocked() }()
	dirs := &dirCache{root: s.root}
	defer dirs.close()
	if err := s.precheck(dirs); err != nil {
		return PublishResult{}, err
	}
	var res PublishResult
	if s.kind == ops.FileKindDir {
		if err := s.mkdirAll(dirs, s.dest); err != nil {
			return res, err
		}
	} else if parent, _ := splitParent(s.dest); parent != "" {
		if err := s.mkdirAll(dirs, parent); err != nil {
			return res, err
		}
	}
	for i := range s.entries {
		e := &s.entries[i]
		full := s.full(e.entry.Path)
		if e.entry.Type == proto.FileTypeDir {
			if err := s.mkdirAll(dirs, full); err != nil {
				return res, err
			}
			res.Published = append(res.Published, ops.FileTransferEntry{Path: full})
			continue
		}
		if err := s.publishFile(dirs, e, full); err != nil {
			return res, err
		}
		res.Files++
		res.Bytes += e.entry.Size
		res.Published = append(res.Published, ops.FileTransferEntry{Path: full, Size: e.entry.Size, SHA256: e.entry.SHA256})
	}
	return res, nil
}

// precheck refuses a transfer when a destination name is taken: an existing
// non-directory where a directory goes, an existing entry where a file goes
// (without Overwrite), or an existing entry that is not a regular file with
// one link (with Overwrite).
func (s *Stager) precheck(dirs *dirCache) error {
	for _, e := range s.entries {
		full := s.full(e.entry.Path)
		parent, name := splitParent(full)
		dir, err := dirs.open(parent)
		if err != nil {
			if isNotExist(err) {
				continue // created by Publish: nothing below it exists
			}
			return openErr(err, true)
		}
		si, err := statAt(dir, name)
		if err != nil {
			if isNotExist(err) {
				continue
			}
			return internal("the destination cannot be read")
		}
		if err := s.taken(e.entry.Type, si); err != nil {
			return err
		}
	}
	return nil
}

func (s *Stager) taken(typ string, si statInfo) error {
	if typ == proto.FileTypeDir {
		if si.Type != typeDir {
			return conflict("a destination folder exists and is not a folder")
		}
		return nil
	}
	if !s.opts.Overwrite {
		return conflict("a destination file exists")
	}
	if si.Type != typeFile || si.Nlink != 1 {
		return conflict("a destination exists and is not a regular file with one link")
	}
	return nil
}

// mkdirAll creates the missing directories of rel one component at a time
// (mkdirat 0755 minus umask) through the root.
func (s *Stager) mkdirAll(dirs *dirCache, rel string) error {
	if rel == "" {
		return nil
	}
	parent, name := splitParent(rel)
	pdir, err := dirs.open(parent)
	if err != nil && isNotExist(err) {
		if err := s.mkdirAll(dirs, parent); err != nil {
			return err
		}
		pdir, err = dirs.open(parent)
	}
	if err != nil {
		return openErr(err, true)
	}
	err = withFd(pdir, func(fd int) error { return sysMkdirAt(fd, name) })
	switch {
	case err == nil:
	case errors.Is(err, errExists):
		si, serr := statAt(pdir, name)
		if serr != nil || si.Type != typeDir {
			return conflict("a destination folder exists and is not a folder")
		}
	case isNoSpace(err):
		return internal("no space left on the receiving file system")
	default:
		return internal("a destination folder cannot be created")
	}
	return nil
}

func (s *Stager) publishFile(dirs *dirCache, e *stagedEntry, full string) error {
	parent, name := splitParent(full)
	dir, err := dirs.open(parent)
	if err != nil {
		return openErr(err, true)
	}
	if beforeRenameHook != nil {
		beforeRenameHook(dir, name)
	}
	err = withFd(s.stageDir, func(from int) error {
		return withFd(dir, func(to int) error {
			if s.opts.Overwrite {
				si, err := sysStatAt(to, name)
				switch {
				case err == nil:
					if terr := s.taken(proto.FileTypeFile, si); terr != nil {
						return terr
					}
					return sysRenameReplace(from, e.tmp, to, name)
				case !isNotExist(err):
					return err
				}
			}
			return sysRenameNoReplace(from, e.tmp, to, name)
		})
	})
	var pe *proto.Error
	switch {
	case err == nil:
		e.done = true
		return nil
	case errors.As(err, &pe):
		return pe
	case errors.Is(err, errExists):
		return conflict("a destination file exists")
	case isXDev(err):
		return typeRefused("the destination is on another file system")
	}
	return internal("a file cannot be published")
}

// Abort removes every staged file that was not published and ends the
// stager. It is safe to call more than once and after Publish.
func (s *Stager) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abortLocked()
}

func (s *Stager) abortLocked() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var first error
	for i := range s.entries {
		e := &s.entries[i]
		if e.tmp == "" || e.done {
			continue
		}
		err := withFd(s.stageDir, func(fd int) error { return sysUnlinkAt(fd, e.tmp) })
		if err != nil && !isNotExist(err) && first == nil {
			first = internal("a staging file cannot be removed")
		}
		e.done = true
	}
	_ = s.stageDir.Close()
	return first
}

// dirCache keeps the last opened destination directory, since entries of one
// directory follow each other in walk order.
type dirCache struct {
	root Root
	rel  string
	f    *os.File
}

func (c *dirCache) open(rel string) (*os.File, error) {
	if c.f != nil && c.rel == rel {
		return c.f, nil
	}
	f, err := c.root.OpenDir(rel)
	if err != nil {
		return nil, err
	}
	c.close()
	c.f, c.rel = f, rel
	return f, nil
}

func (c *dirCache) close() {
	if c.f != nil {
		_ = c.f.Close()
		c.f = nil
	}
}
