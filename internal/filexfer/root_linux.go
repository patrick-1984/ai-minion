//go:build linux

package filexfer

import (
	"errors"
	"io"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux and Android (GOOS android builds the linux files too).

const (
	// noFollowFlags are added to the last element's open.
	noFollowFlags = unix.O_NOFOLLOW
	// walkDirFlags open an intermediate element of a walk: a handle for *at
	// calls, never following a symlink.
	walkDirFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	// upFlags open ".", "..": a handle without read permission.
	upFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC
	// needMountID: the walk compares mount ids (statx STATX_MNT_ID).
	needMountID = true
	// ancestryMayStop: an O_PATH handle of ".." needs no read permission,
	// so the ancestry always reaches the top; an incomplete one is refused.
	ancestryMayStop = false
	// unprobedMode: openat2 and statx are not called in the process before
	// the probe proved them safe.
	unprobedMode = ResolveUnavailable
)

// procDir is the proc file system of the Worker's own namespace.
const procDir = "/proc"

var (
	defaultSystemFolders     = []string{"/", "/bin", "/sbin", "/usr", "/var", "/lib", "/lib64", "/home"}
	defaultSystemDeepFolders = []string{"/proc", "/sys", "/dev", "/etc", "/boot", "/run"}
)

// mountID returns the mount id of fd (statx STATX_MNT_ID, Linux 5.8).
func mountID(fd int) (uint64, bool) {
	var stx unix.Statx_t
	err := ignoringEINTR(func() error {
		return unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &stx)
	})
	if err != nil || stx.Mask&unix.STATX_MNT_ID == 0 {
		return 0, false
	}
	return stx.Mnt_id, true
}

// sameMount reports whether fd is on the root's mount. Without a mount id
// (openat2 on Linux 5.6 and 5.7) RESOLVE_NO_XDEV alone decided.
func (r *Root) sameMount(fd int, _ *unix.Stat_t) bool {
	if r.mnt == 0 {
		return r.mode == ResolveOpenat2
	}
	m, ok := mountID(fd)
	return ok && m == r.mnt
}

// openAbsNoSymlinks opens the clean absolute directory path without following
// any symlink.
func openAbsNoSymlinks(path string, mode ResolveMode) (int, error) {
	if mode == ResolveOpenat2 {
		return openAbsOpenat2(path)
	}
	return openAbsWalk(path)
}

// OpenPaneRoot opens the folder of a pane as a root (BRD FR-2): the pane
// process's working directory "/proc/<pid>/cwd" in its own mount namespace,
// cross-checked against panePath (tmux #{pane_current_path}) opened without
// following symlinks: both handles must have the same identity and mount id.
// A mismatch, an unreadable /proc/<pid>, or a symlink in panePath gives
// files_root_unknown; the floor gives files_root_refused. The handle of
// /proc/<pid>/cwd is the root.
func OpenPaneRoot(pid int, panePath string, opts RootOptions) (*Root, error) {
	return openPaneRootAt(procDir, pid, panePath, opts)
}

func openPaneRootAt(proc string, pid int, panePath string, opts RootOptions) (*Root, error) {
	mode, err := usableMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	if pid <= 0 {
		return nil, rootUnknown("the pane has no process", nil)
	}
	if err := checkAbs(panePath); err != nil {
		return nil, err
	}
	cwd, err := unix.Open(proc+"/"+strconv.Itoa(pid)+"/cwd", upFlags, 0)
	if err != nil {
		return nil, rootUnknown("the working directory of the pane process cannot be read", err)
	}
	byPath, err := openAbsNoSymlinks(panePath, mode)
	if err != nil {
		_ = unix.Close(cwd)
		return nil, err
	}
	same, err := sameObject(cwd, byPath)
	_ = unix.Close(byPath)
	if err != nil || !same {
		_ = unix.Close(cwd)
		return nil, rootUnknown("the folder the pane reports is not the working directory of its process (another mount namespace?)", err)
	}
	cwd, err = applyConfine(cwd, mode, opts)
	if err != nil {
		return nil, err
	}
	return finishRoot(cwd, mode, opts)
}

// sameObject reports whether two handles name the same directory on the same
// mount.
func sameObject(a, b int) (bool, error) {
	sa, err := fstatFd(a)
	if err != nil {
		return false, err
	}
	sb, err := fstatFd(b)
	if err != nil {
		return false, err
	}
	if identityOfStat(&sa) != identityOfStat(&sb) {
		return false, nil
	}
	ma, oka := mountID(a)
	mb, okb := mountID(b)
	if oka != okb || ma != mb {
		return false, nil
	}
	return true, nil
}

// OpenCwdRoot opens the process's current directory as a root (the REST CLI
// caller, BRD FR-4), with the floor checks.
func OpenCwdRoot(opts RootOptions) (*Root, error) {
	mode, err := usableMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(".", upFlags, 0)
	if err != nil {
		return nil, rootUnknown("the current folder cannot be opened", err)
	}
	fd, err = applyConfine(fd, mode, opts)
	if err != nil {
		return nil, err
	}
	return finishRoot(fd, mode, opts)
}

// probeHost: Linux needs the child.
func probeHost() (ProbeResult, bool) { return ProbeResult{}, false }

// probeChildChecks runs the checks of the files probe in the current folder
// (d/f and the symlink l -> d, prepared by Probe): statx with STATX_MNT_ID,
// then openat2 opening d/f and refusing l/f (symlink) and ../ (beneath).
func probeChildChecks(w io.Writer) {
	say := func(s string) { _, _ = io.WriteString(w, s+"\n") }
	say(probeBegin + "statx")
	dot, err := unix.Open(".", upFlags, 0)
	if err != nil {
		say("statx" + probeFail + "the probe folder cannot be opened")
		return
	}
	defer func() { _ = unix.Close(dot) }()
	if _, ok := mountID(dot); ok {
		say("statx" + probeOK)
	} else {
		say("statx" + probeFail + "no mount id")
	}
	say(probeBegin + "openat2")
	say("openat2" + probeOutcome(openat2Check(dot)))
}

func probeOutcome(why string) string {
	if why == "" {
		return probeOK
	}
	return probeFail + why
}

// openat2Check returns "" when openat2 resolves beneath dot as required.
func openat2Check(dot int) string {
	how := unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: resolveBeneath}
	fd, err := unix.Openat2(dot, "d/f", &how)
	if err != nil {
		return "open: " + errnoName(err)
	}
	_ = unix.Close(fd)
	if fd, err = unix.Openat2(dot, "l/f", &how); err == nil {
		_ = unix.Close(fd)
		return "a symlink was followed"
	} else if !errors.Is(err, unix.ELOOP) {
		return "symlink: " + errnoName(err)
	}
	dirHow := unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: resolveBeneath}
	if fd, err = unix.Openat2(dot, "..", &dirHow); err == nil {
		_ = unix.Close(fd)
		return "\"..\" left the folder"
	} else if !errors.Is(err, unix.EXDEV) {
		return "beneath: " + errnoName(err)
	}
	return ""
}

func errnoName(err error) string {
	var e unix.Errno
	if errors.As(err, &e) {
		if n := unix.ErrnoName(e); n != "" {
			return n
		}
	}
	return "error"
}

// openAbsWalk opens the clean absolute directory path from "/" one element
// at a time without following symlinks (mounts may be crossed: this is the
// path to a root, not a path beneath one).
func openAbsWalk(path string) (int, error) {
	if err := checkAbs(path); err != nil {
		return -1, err
	}
	fd, err := unix.Open("/", walkDirFlags, 0)
	if err != nil {
		return -1, rootUnknown("the top folder cannot be opened", err)
	}
	if path == "/" {
		return fd, nil
	}
	for _, e := range strings.Split(path[1:], "/") {
		next, err := openatRetry(fd, e, walkDirFlags, 0)
		if err != nil {
			cerr := classifyOpenError(fd, e, "", err)
			_ = unix.Close(fd)
			var rf *Refusal
			if errors.As(cerr, &rf) {
				return -1, rootUnknown("the path of the folder of the session contains a symbolic link", err)
			}
			return -1, rootUnknown("the folder of the session cannot be opened", err)
		}
		_ = unix.Close(fd)
		fd = next
		st, err := fstatFd(fd)
		if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(fd)
			return -1, rootUnknown("the path of the folder of the session contains a symbolic link or a file", err)
		}
	}
	return fd, nil
}
