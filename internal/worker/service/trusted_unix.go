//go:build linux || darwin || freebsd

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// maxLinks bounds the symbolic links followed while walking one path.
const maxLinks = 40

// walkTrusted walks spec.Path from "/" and returns a handle on the directory.
func walkTrusted(spec DirSpec) (*os.File, error) {
	cur, err := openDirAt(-1, "/", "/")
	if err != nil {
		return nil, err
	}
	parts := splitPath(spec.Path)
	walked := "/"
	links := 0
	for len(parts) > 0 {
		name := parts[0]
		parts = parts[1:]
		next, err := openDirAt(int(cur.Fd()), name, filepath.Join(walked, name))
		created := false
		if isNotExist(err) && spec.Create {
			next, created, err = mkdirAt(cur, name, filepath.Join(walked, name), spec)
		}
		switch {
		case err == nil:
			if created && spec.Chown != nil {
				if cerr := spec.Chown.ChownDir(next, spec.UID, spec.GID); cerr != nil {
					_ = next.Close()
					_ = cur.Close()
					return nil, cerr
				}
			}
			_ = cur.Close()
			cur = next
			walked = filepath.Join(walked, name)
		case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
			rest, rerr := followLink(cur, walked, name, spec)
			if rerr != nil {
				_ = cur.Close()
				return nil, rerr
			}
			if links++; links > maxLinks {
				_ = cur.Close()
				return nil, fmt.Errorf("%s: too many levels of symbolic links", spec.Path)
			}
			if filepath.IsAbs(rest) {
				_ = cur.Close()
				if cur, err = openDirAt(-1, "/", "/"); err != nil {
					return nil, err
				}
				walked = "/"
			}
			parts = append(splitPath(rest), parts...)
		default:
			_ = cur.Close()
			return nil, fmt.Errorf("open %s: %w", filepath.Join(walked, name), err)
		}
	}
	return cur, nil
}

// followLink returns the target of the link name in dir when it is a link
// that the owner may have placed, and an error for everything else.
func followLink(dir *os.File, walked, name string, spec DirSpec) (string, error) {
	p := filepath.Join(walked, name)
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", fmt.Errorf("stat %s: %w", p, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return "", errNotDirectory(p)
	}
	var parent unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &parent); err != nil {
		return "", fmt.Errorf("stat %s: %w", walked, err)
	}
	trusted := int(st.Uid) == spec.LinkOwner && int(parent.Uid) == spec.LinkOwner && parent.Mode&0o022 == 0
	if !trusted {
		return "", errLink(p)
	}
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(int(dir.Fd()), name, buf)
	if err != nil {
		return "", fmt.Errorf("read link %s: %w", p, err)
	}
	target := string(buf[:n])
	for _, part := range strings.Split(target, "/") {
		if part == ".." {
			return "", fmt.Errorf("%s points to %s; links that climb with .. are refused while running as root", p, target)
		}
	}
	return target, nil
}

func openDirAt(dirfd int, name, label string) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	var fd int
	var err error
	if dirfd < 0 {
		fd, err = unix.Open(name, flags, 0)
	} else {
		fd, err = unix.Openat(dirfd, name, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), label), nil
}

// mkdirAt creates name in dir and opens it. created is false when somebody
// else made the directory in the meantime: it is then not ours to give away.
func mkdirAt(dir *os.File, name, label string, spec DirSpec) (f *os.File, created bool, err error) {
	err = unix.Mkdirat(int(dir.Fd()), name, uint32(spec.Mode.Perm()))
	if err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, false, err
	}
	f, oerr := openDirAt(int(dir.Fd()), name, label)
	return f, err == nil, oerr
}
