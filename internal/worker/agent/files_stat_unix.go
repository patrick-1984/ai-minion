//go:build unix

package agent

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
)

// identityOf returns the identity (device, inode) of a file from its fstat.
func identityOf(fi os.FileInfo) (filexfer.Identity, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return filexfer.Identity{}, false
	}
	return filexfer.Identity{Dev: uint64(st.Dev), Ino: st.Ino}, true //nolint:gosec,unconvert // int32 on darwin
}

// fxStat is what a listing and an admission need to know of one entry,
// read with fstatat(AT_SYMLINK_NOFOLLOW) relative to a held directory: the
// name is never resolved from the process's folder.
type fxStat struct {
	mode  uint32 // S_IFMT bits
	size  int64
	mtime time.Time
	nlink uint64
	dev   uint64
	ino   uint64
}

func (s fxStat) isDir() bool     { return s.mode == unix.S_IFDIR }
func (s fxStat) isReg() bool     { return s.mode == unix.S_IFREG }
func (s fxStat) isSymlink() bool { return s.mode == unix.S_IFLNK }

// lstatAt reads the entry name of the directory dir without following a
// symbolic link.
func lstatAt(dir *os.File, name string) (fxStat, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fxStat{}, &os.PathError{Op: "fstatat", Path: "entry", Err: err}
	}
	return fxStat{
		mode:  uint32(st.Mode) & unix.S_IFMT, //nolint:gosec,unconvert // the mode bits fit
		size:  st.Size,
		mtime: time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)), //nolint:unconvert // int32 on some platforms
		nlink: uint64(st.Nlink),                                   //nolint:unconvert // uint16 on darwin
		dev:   uint64(st.Dev),                                     //nolint:gosec,unconvert // int32 on darwin
		ino:   st.Ino,
	}, nil
}

// freeSpace returns the available and the total bytes of the file system of
// the directory descriptor fd.
func freeSpace(fd int) (avail, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)                                    //nolint:gosec,unconvert // the block size fits
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil //nolint:gosec // the counts fit
}
