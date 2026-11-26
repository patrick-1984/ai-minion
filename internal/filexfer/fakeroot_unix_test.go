//go:build linux || darwin

package filexfer

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// testRoot is a Root for tests: a per-component O_NOFOLLOW walk from a held
// directory descriptor with a device check (the real roots are WP2's).
type testRoot struct {
	dir string
	fd  int
	dev uint64
}

func newTestRoot(t *testing.T, dir string) *testRoot {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	st, err := sysFstat(fd)
	if err != nil {
		t.Fatal(err)
	}
	return &testRoot{dir: dir, fd: fd, dev: st.Dev}
}

func (r *testRoot) open(rel string, last int) (*os.File, error) {
	fd, err := unix.Dup(r.fd)
	if err != nil {
		return nil, err
	}
	if rel == "" {
		if last&unix.O_DIRECTORY == 0 {
			_ = unix.Close(fd)
			return nil, unix.EISDIR
		}
		return os.NewFile(uintptr(fd), "."), nil
	}
	elems := strings.Split(rel, "/")
	for i, e := range elems {
		if e == "" || e == "." || e == ".." {
			_ = unix.Close(fd)
			return nil, unix.EINVAL
		}
		flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i == len(elems)-1 {
			flags = last | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		next, err := unix.Openat(fd, e, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		st, err := sysFstat(next)
		if err != nil || st.Dev != r.dev {
			_ = unix.Close(next)
			return nil, unix.EXDEV
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), rel), nil
}

func (r *testRoot) OpenDir(rel string) (*os.File, error) {
	return r.open(rel, unix.O_RDONLY|unix.O_DIRECTORY)
}

func (r *testRoot) OpenFile(rel string) (*os.File, error) {
	return r.open(rel, unix.O_RDONLY|unix.O_NONBLOCK)
}
