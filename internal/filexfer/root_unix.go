//go:build linux || darwin

package filexfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// maxAncestry bounds the upward walks (a path of MaxPathBytes has fewer
// levels).
const maxAncestry = 4096

// maxNamesScanned bounds the entries read from one ancestor to find the name
// of its child (the name is for the display root and the name rules; the
// identity rules do not need it).
const maxNamesScanned = 1 << 16

// identityOfInfo returns the identity of a FileInfo from os.Stat or os.Lstat.
func identityOfInfo(fi os.FileInfo) (Identity, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false
	}
	return Identity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, true //nolint:gosec,unconvert // st_dev is signed on macOS
}

func identityOfStat(st *unix.Stat_t) Identity {
	return Identity{Dev: uint64(st.Dev), Ino: uint64(st.Ino)} //nolint:gosec,unconvert // st_dev is signed on macOS
}

func fstatFd(fd int) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := ignoringEINTR(func() error { return unix.Fstat(fd, &st) })
	return st, err
}

func ignoringEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func openatRetry(dirfd int, name string, flags int, perm uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flags, perm)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

// Close releases the root handle. It is safe to call more than once.
func (r *Root) Close() error {
	if r == nil || !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	return unix.Close(r.fd)
}

// Open opens rel (a wire path, "" for the root itself) beneath the root with
// flags (O_RDONLY, O_WRONLY|O_CREAT|O_EXCL, O_DIRECTORY, ... ; O_CLOEXEC and
// O_NOFOLLOW are added) and perm for a created file. No symlink is followed,
// no mount is crossed and ".." never occurs (the path is checked first). A
// refusal is a *Refusal: path_outside_root for a hostile path,
// file_type_refused with proto.SkipSymlink or proto.SkipMount for a symlink or
// a mount crossing on the way. Other errors keep their errno (errors.Is with
// fs.ErrNotExist, fs.ErrExist, unix.ENOTDIR work). The returned file's Name
// is rel. Type checks of the opened entry (regular file, link count) are the
// caller's: see FR-6.
func (r *Root) Open(rel string, flags int, perm uint32) (*os.File, error) {
	fd, err := r.openFd(rel, flags, perm)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// OpenDir opens the directory rel ("" for the root) beneath the root for
// reading its entries and for *at system calls.
func (r *Root) OpenDir(rel string) (*os.File, error) {
	return r.Open(rel, unix.O_RDONLY|unix.O_DIRECTORY, 0)
}

// OpenParent opens the parent directory of rel beneath the root and returns
// it with the last element of rel, for the *at calls of staging and publish
// (mkdirat, renameat2, linkat, unlinkat, fstatat with AT_SYMLINK_NOFOLLOW),
// which never resolve more than that one element.
func (r *Root) OpenParent(rel string) (dir *os.File, name string, err error) {
	if rel == "" {
		return nil, "", refuse(proto.CodePathOutsideRoot, proto.SkipName, "the root has no parent inside the folder", nil)
	}
	if err := checkRel(rel); err != nil {
		return nil, "", err
	}
	parent, name := "", rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		parent, name = rel[:i], rel[i+1:]
	}
	dir, err = r.OpenDir(parent)
	if err != nil {
		return nil, "", err
	}
	return dir, name, nil
}

func checkRel(rel string) error {
	if err := ops.CheckFilePath(rel, true); err != nil {
		var pe *proto.Error
		msg := "the path is not a valid relative path inside the folder"
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		return refuse(proto.CodePathOutsideRoot, proto.SkipName, msg, err)
	}
	return nil
}

func (r *Root) openFd(rel string, flags int, perm uint32) (int, error) {
	if r == nil || r.closed.Load() {
		return -1, os.ErrClosed
	}
	if err := checkRel(rel); err != nil {
		return -1, err
	}
	var fd int
	var err error
	switch r.mode {
	case ResolveOpenat2:
		fd, err = r.resolveOpenat2(rel, flags, perm)
	case ResolveWalk:
		fd, err = r.resolveWalk(rel, flags, perm)
	default:
		return -1, rootUnknown("no safe path resolution is available on this host", nil)
	}
	if err != nil {
		return -1, err
	}
	st, err := fstatFd(fd)
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		_ = unix.Close(fd)
		return -1, symlinkRefusal(nil)
	}
	if !r.sameMount(fd, &st) {
		_ = unix.Close(fd)
		return -1, mountRefusal(nil)
	}
	return fd, nil
}

func symlinkRefusal(cause error) *Refusal {
	return refuse(proto.CodeFileTypeRefused, proto.SkipSymlink, "the path goes through a symbolic link, which is never followed", cause)
}

func mountRefusal(cause error) *Refusal {
	return refuse(proto.CodeFileTypeRefused, proto.SkipMount, "the path crosses into another file system or a bind mount", cause)
}

// classifyOpenError turns ELOOP, or ENOTDIR caused by a symlink, of opening
// name in dirfd into a symlink refusal; other errors are returned wrapped
// with the relative name.
func classifyOpenError(dirfd int, name, rel string, err error) error {
	if errors.Is(err, unix.ELOOP) {
		return symlinkRefusal(err)
	}
	if errors.Is(err, unix.ENOTDIR) {
		var st unix.Stat_t
		if unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return symlinkRefusal(err)
		}
	}
	if errors.Is(err, unix.EXDEV) {
		return mountRefusal(err)
	}
	return &os.PathError{Op: "open", Path: rel, Err: err}
}

// Encloses reports whether the directory dir lies beneath the root (or is
// the root), by walking upward with ".." and comparing identities. It is the
// check of SEC-1; it does not depend on names, so a renamed root is still
// found.
func (r *Root) Encloses(dir *os.File) (bool, error) {
	if r == nil || r.closed.Load() {
		return false, os.ErrClosed
	}
	sc, err := dir.SyscallConn()
	if err != nil {
		return false, err
	}
	var ok bool
	var werr error
	if err := sc.Control(func(fd uintptr) {
		ok, werr = r.enclosesFd(int(fd)) //nolint:gosec // a descriptor fits in int
	}); err != nil {
		return false, err
	}
	return ok, werr
}

func (r *Root) enclosesFd(start int) (bool, error) {
	st, err := fstatFd(start)
	if err != nil {
		return false, err
	}
	cur, owned := start, false
	defer func() {
		if owned {
			_ = unix.Close(cur)
		}
	}()
	id := identityOfStat(&st)
	for range maxAncestry {
		if id == r.id {
			return true, nil
		}
		up, err := openatRetry(cur, "..", upFlags, 0)
		if err != nil {
			return false, err
		}
		if owned {
			_ = unix.Close(cur)
		}
		cur, owned = up, true
		pst, err := fstatFd(up)
		if err != nil {
			return false, err
		}
		pid := identityOfStat(&pst)
		if pid == id {
			return false, nil // reached the top
		}
		id = pid
	}
	return false, nil
}

// ancestry walks upward from the directory fd to the top and returns the
// identities of the strict ancestors (nearest first) and the path elements
// from the top down to fd (an element whose name cannot be found is "").
// complete is false when an ancestor could not be opened (macOS without read
// permission on an ancestor).
func ancestry(fd int, id Identity) (anc []Identity, names []string, complete bool, err error) {
	cur, owned := fd, false
	defer func() {
		if owned {
			_ = unix.Close(cur)
		}
	}()
	var rev []string
	child := id
	for range maxAncestry {
		up, readable, oerr := openParent(cur)
		if oerr != nil {
			return anc, reverse(rev), false, nil
		}
		if owned {
			_ = unix.Close(cur)
		}
		cur, owned = up, true
		pst, serr := fstatFd(up)
		if serr != nil {
			return nil, nil, false, serr
		}
		pid := identityOfStat(&pst)
		if pid == child {
			return anc, reverse(rev), true, nil // the top: ".." of "/" is "/"
		}
		name := ""
		if readable {
			name = childName(up, child)
		}
		rev = append(rev, name)
		anc = append(anc, pid)
		child = pid
	}
	return anc, reverse(rev), false, nil
}

// openParent opens ".." of fd, readable for its entries when possible.
func openParent(fd int) (up int, readable bool, err error) {
	up, err = openatRetry(fd, "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err == nil {
		return up, true, nil
	}
	if upFlags != unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC {
		up, err = openatRetry(fd, "..", upFlags, 0)
		if err == nil {
			return up, false, nil
		}
	}
	return -1, false, err
}

// childName returns the name of the entry of the directory dirfd whose
// identity is child, or "".
func childName(dirfd int, child Identity) string {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return ""
	}
	f := os.NewFile(uintptr(dup), "..")
	defer func() { _ = f.Close() }()
	scanned := 0
	for scanned < maxNamesScanned {
		names, err := f.Readdirnames(512)
		for _, n := range names {
			var st unix.Stat_t
			if unix.Fstatat(dirfd, n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && identityOfStat(&st) == child {
				return n
			}
		}
		scanned += len(names)
		if err != nil || len(names) == 0 {
			return ""
		}
	}
	return ""
}

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// finishRoot verifies the directory fd as a root: a directory, its mount id
// when the mode needs it, its ancestry, the Confine rule, the floor; and builds
// the display root. It owns fd: on error fd is closed.
func finishRoot(fd int, mode ResolveMode, opts RootOptions) (*Root, error) {
	root, err := buildRoot(fd, mode, opts)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return root, nil
}

func buildRoot(fd int, mode ResolveMode, opts RootOptions) (*Root, error) {
	st, err := fstatFd(fd)
	if err != nil {
		return nil, rootUnknown("the folder of the session cannot be read", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, rootUnknown("the folder of the session is not a folder", nil)
	}
	r := &Root{fd: fd, id: identityOfStat(&st), mode: mode, floor: opts.Floor}
	if mnt, ok := mountID(fd); ok {
		r.mnt = mnt
	} else if mode == ResolveWalk && needMountID {
		return nil, rootUnknown("the kernel cannot report mount ids (statx STATX_MNT_ID), so the folder cannot be confined", nil)
	}
	anc, names, complete, err := ancestry(fd, r.id)
	if err != nil {
		return nil, rootUnknown("the parents of the folder of the session cannot be read", err)
	}
	if !complete && !ancestryMayStop {
		return nil, rootUnknown("the parents of the folder of the session cannot be read", nil)
	}
	r.pushProtected, err = opts.Floor.checkRoot(r.id, anc, names)
	if err != nil {
		return nil, err
	}
	r.display = displayRoot(r.id, anc, names, opts)
	return r, nil
}

// displayRoot builds the display root from the ancestry: relative to the
// Confine folder or home when one of them is the root or an ancestor, else
// the absolute path from the names. It is never used to open anything.
func displayRoot(id Identity, anc []Identity, names []string, opts RootOptions) string {
	label := func(base string, below []string) string {
		if len(below) == 0 {
			return base
		}
		parts := make([]string, len(below))
		for i, n := range below {
			if n == "" {
				n = "?"
			}
			parts[i] = n
		}
		return base + "/" + strings.Join(parts, "/")
	}
	// names[len(names)-1-k] is the element of the object at depth k below the
	// root's ancestors: names[last] is the root, names[last-1] anc[0], ...
	find := func(want Identity) (int, bool) {
		if want.IsZero() {
			return 0, false
		}
		if want == id {
			return len(names), true
		}
		for k, a := range anc {
			if a == want {
				return len(names) - 1 - k, true
			}
		}
		return 0, false
	}
	if opts.Confine != "" {
		if i, ok := find(confineIdentity(opts.Confine)); ok && i <= len(names) {
			l := opts.ConfineLabel
			if l == "" {
				l = filepath.Base(opts.Confine)
			}
			return label(l, names[i:])
		}
	}
	if opts.Floor != nil {
		if i, ok := find(opts.Floor.home); ok && i <= len(names) {
			return label("~", names[i:])
		}
	}
	if len(names) == 0 {
		return "/"
	}
	return label("", names)
}

// confineIdentity returns the identity of the Confine folder opened without
// following symlinks, or the zero Identity.
func confineIdentity(p string) Identity {
	fd, err := openAbsNoSymlinks(p, effectiveMode(ResolveDefault))
	if err != nil {
		return Identity{}
	}
	defer func() { _ = unix.Close(fd) }()
	st, err := fstatFd(fd)
	if err != nil {
		return Identity{}
	}
	return identityOfStat(&st)
}

// applyConfine keeps the pane root fd when it is the Confine folder or below
// it, and otherwise closes it and opens Confine itself.
func applyConfine(fd int, mode ResolveMode, opts RootOptions) (int, error) {
	if opts.Confine == "" {
		return fd, nil
	}
	cfd, err := openAbsNoSymlinks(opts.Confine, mode)
	if err != nil {
		_ = unix.Close(fd)
		return -1, rootUnknown("the confined folder cannot be opened", err)
	}
	cst, err := fstatFd(cfd)
	if err != nil {
		_ = unix.Close(fd)
		_ = unix.Close(cfd)
		return -1, rootUnknown("the confined folder cannot be read", err)
	}
	probe := &Root{fd: fd, id: identityOfStat(&cst)}
	if inside, _ := probe.enclosesFd(fd); inside {
		_ = unix.Close(cfd)
		return fd, nil
	}
	_ = unix.Close(fd)
	return cfd, nil
}

// OpenDirRoot opens the absolute directory path as a root: no element of path
// may be a symlink (files_root_unknown otherwise). It is the root of the
// Android "Minion files" folder and of tests.
func OpenDirRoot(path string, opts RootOptions) (*Root, error) {
	mode, err := usableMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	fd, err := openAbsNoSymlinks(path, mode)
	if err != nil {
		return nil, err
	}
	return finishRoot(fd, mode, opts)
}

// checkAbs requires a clean absolute path.
func checkAbs(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return rootUnknown("the folder of the session is not a clean absolute path", nil)
	}
	return nil
}

// usableMode returns the effective mode or a files_root_unknown refusal.
func usableMode(m ResolveMode) (ResolveMode, error) {
	m = effectiveMode(m)
	switch m {
	case ResolveWalk:
		return m, nil
	case ResolveOpenat2:
		if openat2Supported {
			return m, nil
		}
	}
	return m, rootUnknown("file transfer is not available on this host: no safe path resolution (see minion worker status)", nil)
}
