package filexfer

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// File system primitives of the engine on Linux and Android. Every call is
// relative to a directory file descriptor and names one path element; none
// follows a symbolic link.

// sysSupported reports whether this platform has the engine's primitives.
const sysSupported = true

// forceLinkFallback makes publication use the link+unlink fallback, as on a
// file system without RENAME_NOREPLACE (tests).
var forceLinkFallback bool

func statFrom(st *unix.Stat_t) statInfo {
	si := statInfo{
		Size: st.Size, Nlink: uint64(st.Nlink), Dev: uint64(st.Dev), Ino: st.Ino,
		MTime: time.Unix(st.Mtim.Unix()), CTime: time.Unix(st.Ctim.Unix()),
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		si.Type = typeFile
	case unix.S_IFDIR:
		si.Type = typeDir
	case unix.S_IFLNK:
		si.Type = typeSymlink
	default:
		si.Type = typeOther
	}
	return si
}

func sysStatAt(dirfd int, name string) (statInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return statInfo{}, err
	}
	return statFrom(&st), nil
}

func sysFstat(fd int) (statInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return statInfo{}, err
	}
	return statFrom(&st), nil
}

func sysCreateExcl(dirfd int, name string) (*os.File, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// sysSetMTime sets the access and modification time of an open file on its
// handle (futimens: utimensat with a NULL path).
func sysSetMTime(fd int, mtime time.Time) error {
	ts := [2]unix.Timespec{unix.NsecToTimespec(mtime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	_, _, e := unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// sysRenameNoReplace moves from (in fromfd) to to (in tofd) only when to does
// not exist: renameat2(RENAME_NOREPLACE), or where the file system does not
// support the flag, linkat (which fails with EEXIST) then unlinkat of from.
// An existing to is errExists.
func sysRenameNoReplace(fromfd int, from string, tofd int, to string) error {
	if !forceLinkFallback {
		err := unix.Renameat2(fromfd, from, tofd, to, unix.RENAME_NOREPLACE)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, unix.EEXIST):
			return errExists
		case !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.ENOTSUP):
			return err
		}
	}
	if err := unix.Linkat(fromfd, from, tofd, to, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errExists
		}
		return err
	}
	return unix.Unlinkat(fromfd, from, 0)
}

func sysRenameReplace(fromfd int, from string, tofd int, to string) error {
	return unix.Renameat(fromfd, from, tofd, to)
}

func sysMkdirAt(dirfd int, name string) error {
	err := unix.Mkdirat(dirfd, name, 0o755)
	if errors.Is(err, unix.EEXIST) {
		return errExists
	}
	return err
}

func sysUnlinkAt(dirfd int, name string) error { return unix.Unlinkat(dirfd, name, 0) }

// sysFreeBytes returns the bytes available to an unprivileged user and the
// size of the file system of fd.
func sysFreeBytes(fd int) (avail, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	if st.Frsize > 0 {
		bs = uint64(st.Frsize)
	}
	return st.Bavail * bs, st.Blocks * bs, nil
}

// sysUmask reads the process umask from /proc/self/status (Linux 4.7+)
// without changing it; it falls back to a umask round trip.
func sysUmask() uint32 {
	if f, err := os.Open("/proc/self/status"); err == nil {
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "Umask:"); ok {
				if m, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32); err == nil {
					return uint32(m)
				}
			}
		}
	}
	old := unix.Umask(0o022)
	unix.Umask(old)
	return uint32(old)
}

// sysOpenRootDir opens an absolute directory path one component at a time
// without following symbolic links (cleanup of the staging log).
func sysOpenRootDir(abs string) (*os.File, error) {
	return openAbsNoFollow(abs, func(dirfd int, name string) (int, error) {
		return unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}, func() (int, error) { return unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0) }, unix.Close)
}

func isNotExist(err error) bool { return errors.Is(err, unix.ENOENT) }
func isNoSpace(err error) bool  { return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) }
func isLoop(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EMLINK)
}
func isXDev(err error) bool { return errors.Is(err, unix.EXDEV) }
