//go:build darwin

package filexfer

import (
	"bytes"
	"io"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS: no openat2, no bind mounts for users; the walk with O_NOFOLLOW_ANY
// and st_dev checks is the resolver (BRD 5.3).

const (
	noFollowFlags   = unix.O_NOFOLLOW | unix.O_NOFOLLOW_ANY
	walkDirFlags    = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_NOFOLLOW_ANY | unix.O_CLOEXEC
	upFlags         = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	needMountID     = false
	ancestryMayStop = false
	unprobedMode    = ResolveWalk

	openat2Supported = false
)

var (
	defaultSystemFolders     = []string{"/", "/bin", "/sbin", "/usr", "/var", "/private", "/private/var", "/Library", "/Users", "/Applications", "/Volumes"}
	defaultSystemDeepFolders = []string{"/System", "/private/etc", "/dev", "/cores"}
)

// mountID: macOS has no mount ids; st_dev identifies the volume.
func mountID(int) (uint64, bool) { return 0, false }

// sameMount reports whether fd is on the root's volume (st_dev).
func (r *Root) sameMount(_ int, st *unix.Stat_t) bool {
	return uint64(st.Dev) == r.id.Dev //nolint:gosec // st_dev is signed on macOS
}

func (r *Root) resolveOpenat2(string, int, uint32) (int, error) {
	return -1, rootUnknown("openat2 is not available on macOS", nil)
}

func openAbsNoSymlinks(path string, _ ResolveMode) (int, error) {
	if err := checkAbs(path); err != nil {
		return -1, err
	}
	fd, err := openatRetry(unix.AT_FDCWD, path, walkDirFlags, 0)
	if err != nil {
		return -1, rootUnknown("the folder of the session cannot be opened without following a symbolic link", err)
	}
	return fd, nil
}

// fdPath returns fcntl(F_GETPATH) of fd. x/sys/unix has no F_GETPATH
// wrapper; FcntlFstore passes its pointer argument unchanged to the libSystem
// fcntl wrapper, so it carries the MAXPATHLEN buffer (8-byte aligned, larger
// than Fstore_t).
func fdPath(fd int) (string, error) {
	words := make([]int64, (unix.PathMax+8)/8)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)                                              //nolint:gosec // a byte view of the buffer
	if err := unix.FcntlFstore(uintptr(fd), unix.F_GETPATH, (*unix.Fstore_t)(unsafe.Pointer(&words[0]))); err != nil { //nolint:gosec // see above
		return "", err
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return "", unix.ENAMETOOLONG
}

// OpenPaneRoot opens the folder of a pane as a root (BRD FR-2, macOS): tmux
// reports the kernel's canonical vnode path; it is opened with
// O_DIRECTORY|O_NOFOLLOW_ANY and fcntl(F_GETPATH) of the handle must return
// the same path. A symlink anywhere in the path or a mismatch gives
// files_root_unknown. pid is not used on macOS.
func OpenPaneRoot(_ int, panePath string, opts RootOptions) (*Root, error) {
	mode, err := usableMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	fd, err := openAbsNoSymlinks(panePath, mode)
	if err != nil {
		return nil, err
	}
	got, err := fdPath(fd)
	if err != nil || got != panePath {
		_ = unix.Close(fd)
		return nil, rootUnknown("the folder the pane reports is not the folder that was opened", err)
	}
	fd, err = applyConfine(fd, mode, opts)
	if err != nil {
		return nil, err
	}
	return finishRoot(fd, mode, opts)
}

// OpenCwdRoot opens the process's current directory as a root (the REST CLI
// caller, BRD FR-4). The handle of "." is the directory itself, whatever path
// led there, so no path check is needed; the floor checks apply.
func OpenCwdRoot(opts RootOptions) (*Root, error) {
	mode, err := usableMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	fd, err := openatRetry(unix.AT_FDCWD, ".", upFlags, 0)
	if err != nil {
		return nil, rootUnknown("the current folder cannot be opened", err)
	}
	fd, err = applyConfine(fd, mode, opts)
	if err != nil {
		return nil, err
	}
	return finishRoot(fd, mode, opts)
}

// probeHost: macOS needs no child; the walk is the resolver.
func probeHost() (ProbeResult, bool) { return ProbeResult{Mode: ResolveWalk, MountID: "st_dev"}, true }

func probeChildChecks(w io.Writer) { _, _ = io.WriteString(w, "walk"+probeOK+"\n") }
