package filexfer

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Root is the pinned, verified root of one end (BRD FR-2, 5.3). The root files
// of this package (WP2) implement it with openat2 RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS | RESOLVE_NO_XDEV | RESOLVE_NO_MAGICLINKS on Linux and
// Android, the per-component O_NOFOLLOW walk with device checks on macOS.
// The engine never opens a path in any other way.
type Root interface {
	// OpenDir opens the directory rel beneath the root ("" is the root
	// itself) for reading, so that it can be listed and used as the directory
	// of *at calls (not O_PATH). rel is a checked wire path. A symbolic link
	// in any component is an error that matches syscall.ELOOP or
	// syscall.ENOTDIR, a mount crossing one that matches syscall.EXDEV, an
	// absent directory one that matches os.ErrNotExist (ENOENT).
	OpenDir(rel string) (*os.File, error)
	// OpenFile opens the file rel beneath the root read-only with
	// O_NONBLOCK|O_NOFOLLOW|O_CLOEXEC, under the same rules. The engine
	// checks the type, link count and device of the result itself.
	OpenFile(rel string) (*os.File, error)
}

var (
	errExists      = errors.New("filexfer: the name exists")
	errUnsupported = proto.NewError(proto.CodeFilesRootUnknown, "file transfer is not available on this platform in this version")
)

// Entry types of statInfo.
const (
	typeFile = iota + 1
	typeDir
	typeSymlink
	typeOther
)

// statInfo is the part of a stat result the engine uses.
type statInfo struct {
	Type         int
	Size         int64
	Nlink        uint64
	Dev, Ino     uint64
	MTime, CTime time.Time
}

// withFd runs fn with the descriptor of f, keeping f open meanwhile.
func withFd(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

func fstatFile(f *os.File) (statInfo, error) {
	var si statInfo
	err := withFd(f, func(fd int) error {
		var err error
		si, err = sysFstat(fd)
		return err
	})
	return si, err
}

func statAt(dir *os.File, name string) (statInfo, error) {
	var si statInfo
	err := withFd(dir, func(fd int) error {
		var err error
		si, err = sysStatAt(fd, name)
		return err
	})
	return si, err
}

// Entry is one entry of a folder walk.
type Entry struct {
	// Path is relative to the walked folder (the archive and manifest path).
	Path string
	// Type is proto.FileTypeFile or proto.FileTypeDir.
	Type  string
	Size  int64
	MTime time.Time
	// Dev and Ino identify the entry (identity sets, FR-3).
	Dev, Ino uint64
}

// Skip is an entry a walk left out, with its proto.Skip* reason.
type Skip struct {
	Path   string
	Reason string
}

// WalkOptions control a folder walk.
type WalkOptions struct {
	Limits Limits
	// Filter is asked for every entry that could be transferred, with its
	// path relative to the root (the walked folder joined with Entry.Path),
	// and returns a proto.Skip* reason to leave it out (for a directory: with
	// everything below it), or "" to keep it. The Worker uses it for the
	// pull-sensitive and push-protected lists and the identity sets.
	Filter func(rootRel string, e Entry) string
}

// WalkStats are the totals of a walk.
type WalkStats struct {
	Files, Dirs, Bytes int64
	// Skipped counts all skipped entries; Skips holds the first
	// proto.MaxResultEntries of them.
	Skipped int64
	Skips   []Skip
}

func (s *WalkStats) skip(p, reason string) {
	s.Skipped++
	if len(s.Skips) < proto.MaxResultEntries {
		s.Skips = append(s.Skips, Skip{Path: p, Reason: reason})
	}
}

// ErrSourceChanged is returned when an entry changed between its listing
// and its reading (another type, a replaced directory, a file whose size,
// modification or change time changed while it was read). The transfer fails;
// nothing it sent can be verified.
var ErrSourceChanged = proto.NewError(proto.CodeConflict, "a file changed while it was being sent")

// visitFunc receives each kept entry in walk order. For a file, f is the open
// file (fstat checked: regular, one link, same device); the walker closes it
// after visit returns.
type visitFunc func(e Entry, f *os.File) error

// Walk walks the folder base (a wire path beneath root, "" for the root) in
// walk order: names sorted by byte order per directory, depth first, each
// directory before its contents. Only regular files with one link and
// directories on the root's device are visited; symbolic links, special
// files, hard links, other mounts, names that are not valid wire elements and
// entries the filter refuses are skipped with a reason. The walk enforces the
// entry, byte, depth and path limits (transfer_too_large). When open is
// false, files are not opened (counting for an admission) and visit gets a
// nil file.
func Walk(ctx context.Context, root Root, base string, opts WalkOptions, open bool, visit visitFunc) (WalkStats, error) {
	if !sysSupported {
		return WalkStats{}, errUnsupported
	}
	opts.Limits = opts.Limits.Normalize()
	if err := CheckPath(base, true); err != nil {
		return WalkStats{}, err
	}
	dir, err := root.OpenDir(base)
	if err != nil {
		return WalkStats{}, openErr(err, true)
	}
	defer func() { _ = dir.Close() }()
	st, err := fstatFile(dir)
	if err != nil {
		return WalkStats{}, internal("the folder cannot be read")
	}
	if st.Type != typeDir {
		return WalkStats{}, typeRefused("the path is not a folder")
	}
	w := &walker{ctx: ctx, root: root, base: base, opts: opts, open: open, visit: visit, dev: st.Dev}
	err = w.dir(dir, "", 0)
	return w.stats, err
}

// openErr maps an open error of Root to a proto error.
func openErr(err error, dir bool) error {
	var pe *proto.Error
	switch {
	case errors.As(err, &pe):
		return pe
	case isNotExist(err):
		return proto.NewError(proto.CodeNotFound, "the path does not exist")
	case isLoop(err):
		return typeRefused("the path contains a symbolic link")
	case isXDev(err):
		return typeRefused("the path crosses into another file system")
	case errors.Is(err, os.ErrPermission):
		return proto.NewError(proto.CodeForbidden, "the path cannot be read (permission denied)")
	}
	if dir {
		return internal("the folder cannot be opened")
	}
	return internal("the file cannot be opened")
}

type walker struct {
	ctx   context.Context
	root  Root
	base  string
	opts  WalkOptions
	open  bool
	visit visitFunc
	dev   uint64
	stats WalkStats
}

// readNames reads the names of dir, at most budget of them (more is
// transfer_too_large), sorted by byte order.
func readNames(dir *os.File, budget int64) ([]string, error) {
	var names []string
	for {
		batch, err := dir.Readdirnames(1024)
		names = append(names, batch...)
		if int64(len(names)) > budget {
			return nil, tooLarge("the folder has more entries than the limit")
		}
		if err == io.EOF || (err == nil && len(batch) == 0) {
			break
		}
		if err != nil {
			return nil, internal("a folder cannot be listed")
		}
	}
	slices.Sort(names)
	return names, nil
}

func (w *walker) entries() int64 { return w.stats.Files + w.stats.Dirs }

func (w *walker) dir(dir *os.File, rel string, depth int) error {
	// The budget of names is generous (skipped names do not count as
	// entries) but bounded, so that one huge directory cannot exhaust memory.
	names, err := readNames(dir, 2*w.opts.Limits.MaxEntries)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		p := JoinPath(rel, name)
		if err := w.entry(dir, p, name, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) entry(dir *os.File, p, name string, depth int) error {
	full := JoinPath(w.base, p)
	if !validName(name) || CheckPath(full, false) != nil {
		w.stats.skip(p, proto.SkipName)
		return nil
	}
	si, err := statAt(dir, name)
	if err != nil {
		if isNotExist(err) {
			return nil // removed meanwhile
		}
		return internal("an entry of the folder cannot be read")
	}
	switch {
	case si.Type == typeSymlink:
		w.stats.skip(p, proto.SkipSymlink)
		return nil
	case si.Type == typeOther:
		w.stats.skip(p, proto.SkipSpecial)
		return nil
	case si.Dev != w.dev:
		w.stats.skip(p, proto.SkipMount)
		return nil
	case si.Type == typeFile && si.Nlink != 1:
		w.stats.skip(p, proto.SkipHardlink)
		return nil
	}
	e := Entry{Path: p, Type: proto.FileTypeFile, Size: si.Size, MTime: si.MTime, Dev: si.Dev, Ino: si.Ino}
	if si.Type == typeDir {
		e.Type, e.Size = proto.FileTypeDir, 0
	}
	if w.opts.Filter != nil {
		if reason := w.opts.Filter(full, e); reason != "" {
			w.stats.skip(p, reason)
			return nil
		}
	}
	if err := w.opts.Limits.checkPathLimits(full); err != nil {
		return err
	}
	if depth > w.opts.Limits.MaxDepth {
		return tooLarge("the folder is deeper than the limit")
	}
	if w.entries()+1 > w.opts.Limits.MaxEntries {
		return tooLarge("the folder has more entries than the limit")
	}
	if e.Type == proto.FileTypeDir {
		return w.subdir(e, full, depth)
	}
	if w.stats.Bytes+e.Size > w.opts.Limits.MaxBytes {
		return tooLarge("the folder has more bytes than the limit")
	}
	if !w.open {
		w.stats.Files++
		w.stats.Bytes += e.Size
		return w.visit(e, nil)
	}
	f, err := w.root.OpenFile(full)
	if err != nil {
		if reason := skipReason(err); reason != "" {
			w.stats.skip(p, reason)
			return nil
		}
		return openErr(err, false)
	}
	defer func() { _ = f.Close() }()
	fs, err := fstatFile(f)
	if err != nil {
		return internal("a file cannot be read")
	}
	if fs.Type != typeFile || fs.Dev != si.Dev || fs.Ino != si.Ino {
		return ErrSourceChanged
	}
	if fs.Nlink != 1 {
		w.stats.skip(p, proto.SkipHardlink)
		return nil
	}
	e.Size, e.MTime = fs.Size, fs.MTime
	if w.stats.Bytes+e.Size > w.opts.Limits.MaxBytes {
		return tooLarge("the folder has more bytes than the limit")
	}
	w.stats.Files++
	w.stats.Bytes += e.Size
	return w.visit(e, f)
}

func (w *walker) subdir(e Entry, full string, depth int) error {
	sub, err := w.root.OpenDir(full)
	if err != nil {
		if reason := skipReason(err); reason != "" {
			w.stats.skip(e.Path, reason)
			return nil
		}
		return openErr(err, true)
	}
	defer func() { _ = sub.Close() }()
	ds, err := fstatFile(sub)
	if err != nil {
		return internal("a folder cannot be read")
	}
	if ds.Type != typeDir || ds.Dev != e.Dev || ds.Ino != e.Ino {
		return ErrSourceChanged
	}
	w.stats.Dirs++
	if err := w.visit(e, nil); err != nil {
		return err
	}
	return w.dir(sub, e.Path, depth)
}

// skipReason maps an open error of Root that a folder walk skips over.
func skipReason(err error) string {
	switch {
	case isLoop(err):
		return proto.SkipSymlink
	case isXDev(err):
		return proto.SkipMount
	case isNotExist(err):
		return proto.SkipName
	}
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.CodeFileTypeRefused {
		return proto.SkipSpecial
	}
	return ""
}

// SourceFile is a single regular file opened for sending.
type SourceFile struct {
	File *os.File
	// Size, MTime and CTime are from fstat at opening; Dev and Ino identify
	// the file (ETag inputs, BRD 5.7).
	Size         int64
	MTime, CTime time.Time
	Dev, Ino     uint64
}

// OpenSourceFile opens rel beneath root for a single-file send and checks it
// with fstat: a regular file (file_type_refused otherwise, also for a
// directory) with one link (file_type_refused, hard link).
func OpenSourceFile(root Root, rel string) (*SourceFile, error) {
	if !sysSupported {
		return nil, errUnsupported
	}
	if err := CheckPath(rel, false); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(rel)
	if err != nil {
		return nil, openErr(err, false)
	}
	st, err := fstatFile(f)
	if err != nil {
		_ = f.Close()
		return nil, internal("the file cannot be read")
	}
	switch {
	case st.Type != typeFile:
		_ = f.Close()
		return nil, typeRefused("the path is not a regular file")
	case st.Nlink != 1:
		_ = f.Close()
		return nil, typeRefused("the file has more than one link (hardlink)")
	}
	return &SourceFile{File: f, Size: st.Size, MTime: st.MTime, CTime: st.CTime, Dev: st.Dev, Ino: st.Ino}, nil
}

// Changed reports whether the file's size, modification or change time or
// identity changed since it was opened (re-fstat at the end of serving).
func (s *SourceFile) Changed() (bool, error) {
	st, err := fstatFile(s.File)
	if err != nil {
		return true, err
	}
	return st.Size != s.Size || !st.MTime.Equal(s.MTime) || !st.CTime.Equal(s.CTime) || st.Ino != s.Ino || st.Dev != s.Dev, nil
}

// Close closes the file.
func (s *SourceFile) Close() error { return s.File.Close() }
