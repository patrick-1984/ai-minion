package filexfer

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// File system primitives of the engine on macOS. Every call is relative to a
// directory file descriptor and names one path element; none follows a
// symbolic link.

// sysSupported reports whether this platform has the engine's primitives.
const sysSupported = true

func statFrom(st *unix.Stat_t) statInfo {
	si := statInfo{
		Size: st.Size, Nlink: uint64(st.Nlink), Dev: uint64(uint32(st.Dev)), Ino: st.Ino,
		MTime: time.Unix(st.Mtim.Unix()), CTime: time.Unix(st.Ctim.Unix()),
	}
	switch uint32(st.Mode) & unix.S_IFMT {
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
// handle (futimes, microsecond precision).
func sysSetMTime(fd int, mtime time.Time) error {
	tv := unix.NsecToTimeval(mtime.UnixNano())
	return unix.Futimes(fd, []unix.Timeval{tv, tv})
}

// sysRenameNoReplace moves from (in fromfd) to to (in tofd) only when to does
// not exist: renameatx_np(RENAME_EXCL). An existing to is errExists.
func sysRenameNoReplace(fromfd int, from string, tofd int, to string) error {
	err := unix.RenameatxNp(fromfd, from, tofd, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.EEXIST) {
		return errExists
	}
	return err
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
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

// sysUmask returns the process umask by a round trip (macOS has no way to
// read it without setting it).
func sysUmask() uint32 {
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
