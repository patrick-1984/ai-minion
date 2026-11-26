//go:build linux

package filexfer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Child modes of the test binary.
const (
	envProbeChild = "FILEXFER_TEST_PROBE_CHILD" // run ProbeChild; value: "", or a seccomp filter
	envBindChild  = "FILEXFER_TEST_BIND_CHILD"  // run the bind-mount checks in a new user+mount namespace
)

func TestMain(m *testing.M) {
	if v, ok := os.LookupEnv(envProbeChild); ok {
		if v != "" {
			if err := installSeccomp(v); err != nil {
				fmt.Println("seccomp:", err)
				os.Exit(3)
			}
		}
		os.Exit(ProbeChild(os.Stdout))
	}
	if v, ok := os.LookupEnv(envBindChild); ok {
		os.Exit(bindChild(v))
	}
	os.Exit(m.Run())
}

// openat2Works calls openat2 in the test process (tests may; the Worker
// relies on the probe instead).
func openat2Works() bool {
	how := unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: resolveBeneath}
	fd, err := unix.Openat2(unix.AT_FDCWD, ".", &how)
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}

// installSeccomp installs a filter that answers the system call in spec
// ("<syscall>:<action>", syscall openat2 or statx, action kill, trap or
// enosys) and allows everything else, for all threads of the process.
func installSeccomp(spec string) error {
	name, action, _ := strings.Cut(spec, ":")
	var nr uint32
	switch name {
	case "openat2":
		nr = unix.SYS_OPENAT2
	case "statx":
		nr = unix.SYS_STATX
	default:
		return fmt.Errorf("unknown syscall %q", name)
	}
	var ret uint32
	switch action {
	case "kill":
		ret = unix.SECCOMP_RET_KILL_PROCESS
	case "trap":
		ret = unix.SECCOMP_RET_TRAP
	case "enosys":
		ret = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	const (
		ldAbsW = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeqK   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		retK   = unix.BPF_RET | unix.BPF_K
	)
	filter := []unix.SockFilter{
		{Code: ldAbsW, K: 0}, // seccomp_data.nr
		{Code: jeqK, Jt: 0, Jf: 1, K: nr},
		{Code: retK, K: ret},
		{Code: retK, K: unix.SECCOMP_RET_ALLOW},
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	if _, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&prog))); e != 0 {
		return e
	}
	return nil
}

func probeCommand(seccomp string) ProbeCommand {
	return ProbeCommand{Path: os.Args[0], Args: []string{"-test.run=^$"}, Env: append(os.Environ(), envProbeChild+"="+seccomp)}
}

func TestProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	want := ResolveWalk
	if openat2Works() {
		want = ResolveOpenat2
	}
	scratch := tempDir(t)
	r := Probe(ctx, probeCommand(""), scratch)
	if r.Mode != want || r.MountID != "ok" {
		t.Fatalf("probe = %+v, want mode %v", r, want)
	}
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Fatalf("the probe left %d entries in its scratch folder", len(entries))
	}
	for _, c := range []struct {
		seccomp string
		mode    ResolveMode
		openat2 string
	}{
		{"openat2:kill", ResolveWalk, "killed"},
		{"openat2:trap", ResolveWalk, "killed"},
		{"openat2:enosys", ResolveWalk, "failed: open: ENOSYS"},
		{"statx:kill", ResolveUnavailable, ""},
		{"statx:enosys", want, ""},
	} {
		r := Probe(ctx, probeCommand(c.seccomp), scratch)
		if strings.HasPrefix(c.seccomp, "openat2") && !openat2Works() {
			continue
		}
		if c.seccomp == "statx:enosys" && want == ResolveWalk {
			c.mode = ResolveUnavailable
		}
		if r.Mode != c.mode || (c.openat2 != "" && r.Openat2 != c.openat2) {
			t.Errorf("probe under seccomp %s = %+v, want mode %v openat2 %q", c.seccomp, r, c.mode, c.openat2)
		}
		if r.Mode == ResolveUnavailable && r.Reason == "" {
			t.Errorf("probe under seccomp %s: no reason", c.seccomp)
		}
	}
	// A child that cannot start.
	if r := Probe(ctx, ProbeCommand{Path: filepath.Join(scratch, "missing")}, scratch); r.Mode != ResolveUnavailable || r.Reason == "" {
		t.Fatalf("probe of a missing binary = %+v", r)
	}
}

func TestApplyProbe(t *testing.T) {
	old := DefaultResolveMode()
	defer SetDefaultResolveMode(old)
	ApplyProbe(ProbeResult{Mode: ResolveWalk})
	if DefaultResolveMode() != ResolveWalk {
		t.Fatal("ApplyProbe")
	}
	ApplyProbe(ProbeResult{Mode: ResolveUnavailable})
	if _, err := OpenDirRoot(tempDir(t), RootOptions{}); codeOf(err) != proto.CodeFilesRootUnknown {
		t.Fatalf("unavailable: %v", err)
	}
}

// TestPaneRootNamespaceMismatch simulates a pane in another mount namespace
// with a different /proc view: /proc/<pid>/cwd is another folder than the
// path tmux reports.
func TestPaneRootNamespaceMismatch(t *testing.T) {
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			base := tempDir(t)
			reported := filepath.Join(base, "host", "app")
			actual := filepath.Join(base, "container", "app")
			mustMkdir(t, reported)
			mustMkdir(t, actual)
			proc := filepath.Join(base, "proc")
			mustMkdir(t, filepath.Join(proc, "4242"))
			mustSymlink(t, actual, filepath.Join(proc, "4242", "cwd"))
			if _, err := openPaneRootAt(proc, 4242, reported, RootOptions{Mode: mode}); codeOf(err) != proto.CodeFilesRootUnknown {
				t.Fatalf("mismatch: %v", err)
			}
			r, err := openPaneRootAt(proc, 4242, actual, RootOptions{Mode: mode})
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			_ = r.Close()
			if _, err := openPaneRootAt(proc, 4343, actual, RootOptions{Mode: mode}); codeOf(err) != proto.CodeFilesRootUnknown {
				t.Fatalf("unreadable /proc/<pid>: %v", err)
			}
			if _, err := OpenPaneRoot(0, actual, RootOptions{Mode: mode}); codeOf(err) != proto.CodeFilesRootUnknown {
				t.Fatalf("pid 0: %v", err)
			}
		})
	}
}

// TestResolveMagicLink: /proc/<pid> as a root; its cwd, exe and fd/N are
// magic links and are never followed.
func TestResolveMagicLink(t *testing.T) {
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			r, err := OpenDirRoot("/proc/"+strconv.Itoa(os.Getpid()), RootOptions{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			for _, p := range []string{"cwd", "cwd/x", "exe", "root/etc/passwd", "fd/0"} {
				f, err := r.Open(p, unix.O_RDONLY|unix.O_NONBLOCK, 0)
				if err == nil {
					_ = f.Close()
					t.Errorf("open %s succeeded", p)
					continue
				}
				if codeOf(err) != proto.CodeFileTypeRefused {
					t.Errorf("open %s: %v", p, err)
				}
			}
		})
	}
}

// TestBindMountOfSSH runs in a new user and mount namespace (skipped where
// unprivileged user namespaces are not allowed): a bind mount of ~/.ssh into
// the project folder is refused by the mount checks, and a pane inside the
// bind mount is refused by the identity of ~/.ssh.
func TestBindMountOfSSH(t *testing.T) {
	h := makeHome(t)
	mustWrite(t, filepath.Join(h.ssh, "id_ed25519"), "secret")
	bind := filepath.Join(h.app, "keys")
	mustMkdir(t, bind)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envBindChild+"="+strings.Join([]string{h.home, h.state, h.app, h.ssh, bind}, "\n"))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Skipf("no unprivileged user and mount namespaces here: %v", err)
	}
	if err != nil {
		t.Fatalf("bind child: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "skip:") {
		t.Skip(strings.TrimSpace(string(out)))
	}
}

// bindChild is the body of TestBindMountOfSSH inside the namespaces.
func bindChild(spec string) int {
	p := strings.Split(spec, "\n")
	if len(p) != 5 {
		fmt.Println("bad spec")
		return 2
	}
	home, state, app, ssh, bind := p[0], p[1], p[2], p[3], p[4]
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Println("skip: mount propagation:", err)
		return 0
	}
	if err := unix.Mount(ssh, bind, "", unix.MS_BIND, ""); err != nil {
		fmt.Println("skip: bind mount:", err)
		return 0
	}
	floor := NewFloor(FloorConfig{Home: home, StateDir: state})
	modes := []ResolveMode{ResolveWalk}
	if openat2Works() {
		modes = append(modes, ResolveOpenat2)
	}
	for _, mode := range modes {
		r, err := OpenDirRoot(app, RootOptions{Mode: mode, Floor: floor})
		if err != nil {
			fmt.Println(mode, "open app:", err)
			return 1
		}
		_, err = r.Open("keys/id_ed25519", unix.O_RDONLY, 0)
		_ = r.Close()
		if codeOf(err) != proto.CodeFileTypeRefused || skipOf(err) != proto.SkipMount {
			fmt.Println(mode, "read through the bind mount:", err)
			return 1
		}
		if _, err := OpenDirRoot(bind, RootOptions{Mode: mode, Floor: floor}); codeOf(err) != proto.CodeFilesRootRefused {
			fmt.Println(mode, "root in the bind mount:", err)
			return 1
		}
	}
	return 0
}
