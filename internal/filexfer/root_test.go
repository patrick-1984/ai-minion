//go:build linux || darwin

package filexfer

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The tests run on Linux (both resolvers) and macOS (the walk). Every file
// system object they touch is inside t.TempDir(), except read-only opens of
// "/", "/etc", "/proc" and "/dev" for the floor and mount checks.

// tempDir returns a canonical (symlink-free) temporary folder: on macOS the
// temporary folder lives behind the /var -> /private/var symlink.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// testModes are the resolvers to test on this host.
func testModes(t *testing.T) []ResolveMode {
	t.Helper()
	if runtime.GOOS != "linux" {
		return []ResolveMode{ResolveWalk}
	}
	modes := []ResolveMode{ResolveWalk}
	if openat2Works() {
		modes = append(modes, ResolveOpenat2)
	} else {
		t.Log("openat2 is not available on this kernel; only the walk is tested")
	}
	return modes
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func codeOf(err error) proto.Code {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func skipOf(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Skip
	}
	return ""
}

// sampleTree builds root/{a/b.txt, a/up -> ../.., link -> /etc, inlink -> a,
// abs -> <outside>/secret, sub/} and outside/secret.
func sampleTree(t *testing.T) (root, outside string) {
	t.Helper()
	base := tempDir(t)
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	mustWrite(t, filepath.Join(root, "a", "b.txt"), "inside")
	mustWrite(t, filepath.Join(outside, "secret"), "outside")
	mustMkdir(t, filepath.Join(root, "sub"))
	mustSymlink(t, "../..", filepath.Join(root, "a", "up"))
	mustSymlink(t, "/etc", filepath.Join(root, "link"))
	mustSymlink(t, "a", filepath.Join(root, "inlink"))
	mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(root, "abs"))
	return root, outside
}

func TestResolveTable(t *testing.T) {
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			rootDir, _ := sampleTree(t)
			r, err := OpenDirRoot(rootDir, RootOptions{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if r.Mode() != mode {
				t.Fatalf("mode %v, want %v", r.Mode(), mode)
			}

			f, err := r.Open("a/b.txt", unix.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				t.Fatalf("open a/b.txt: %v", err)
			}
			b, _ := io.ReadAll(f)
			_ = f.Close()
			if string(b) != "inside" {
				t.Fatalf("read %q", b)
			}
			d, err := r.OpenDir("")
			if err != nil {
				t.Fatalf("open the root: %v", err)
			}
			_ = d.Close()

			cases := []struct {
				path string
				code proto.Code
				skip string
			}{
				{"../x", proto.CodePathOutsideRoot, proto.SkipName},
				{"/etc/passwd", proto.CodePathOutsideRoot, proto.SkipName},
				{"a/../../b", proto.CodePathOutsideRoot, proto.SkipName},
				{"a/./b.txt", proto.CodePathOutsideRoot, proto.SkipName},
				{`C:\x`, proto.CodePathOutsideRoot, proto.SkipName},
				{`a\..\b`, proto.CodePathOutsideRoot, proto.SkipName},
				{"a/b.txt\x00", proto.CodePathOutsideRoot, proto.SkipName},
				{strings.Repeat("x", 256), proto.CodePathOutsideRoot, proto.SkipName},
				{"a//b.txt", proto.CodePathOutsideRoot, proto.SkipName},
				{"a/b.txt/", proto.CodePathOutsideRoot, proto.SkipName},
				{proto.StagingPrefix + "x", proto.CodePathOutsideRoot, proto.SkipName},
				{"link", proto.CodeFileTypeRefused, proto.SkipSymlink},
				{"link/passwd", proto.CodeFileTypeRefused, proto.SkipSymlink},
				{"inlink/b.txt", proto.CodeFileTypeRefused, proto.SkipSymlink},
				{"inlink", proto.CodeFileTypeRefused, proto.SkipSymlink},
				{"a/up/outside/secret", proto.CodeFileTypeRefused, proto.SkipSymlink},
				{"abs", proto.CodeFileTypeRefused, proto.SkipSymlink},
			}
			for _, c := range cases {
				for _, flags := range []int{unix.O_RDONLY, unix.O_RDONLY | unix.O_DIRECTORY} {
					f, err := r.Open(c.path, flags, 0)
					if err == nil {
						_ = f.Close()
						t.Errorf("open %q (flags %#x) succeeded", c.path, flags)
						continue
					}
					if codeOf(err) != c.code || skipOf(err) != c.skip {
						t.Errorf("open %q (flags %#x): %v (code %q skip %q), want %q %q", c.path, flags, err, codeOf(err), skipOf(err), c.code, c.skip)
					}
					if strings.Contains(err.Error(), rootDir) {
						t.Errorf("error names the absolute root: %v", err)
					}
				}
			}

			if _, err := r.Open("missing", unix.O_RDONLY, 0); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("missing: %v", err)
			}
			if got := ProtoError(&os.PathError{Op: "open", Path: "x", Err: unix.ENOENT}).Code; got != proto.CodeNotFound {
				t.Errorf("ProtoError(ENOENT) = %s", got)
			}

			// Create, exclusive create, parent handles.
			nf, err := r.Open("sub/new", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			_ = nf.Close()
			if _, err := r.Open("sub/new", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("second exclusive create: %v", err)
			}
			if _, err := r.Open("inlink/new", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600); codeOf(err) != proto.CodeFileTypeRefused {
				t.Fatalf("create through a symlink: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(rootDir, "a", "new")); err == nil {
				t.Fatal("a file was created through a symlink")
			}
			pd, name, err := r.OpenParent("sub/new")
			if err != nil || name != "new" {
				t.Fatalf("OpenParent: %v %q", err, name)
			}
			var st unix.Stat_t
			if err := unix.Fstatat(int(pd.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil { //nolint:gosec // test
				t.Fatal(err)
			}
			if ok, err := r.Encloses(pd); err != nil || !ok {
				t.Fatalf("Encloses(sub) = %v %v", ok, err)
			}
			_ = pd.Close()
			if _, _, err := r.OpenParent(""); codeOf(err) != proto.CodePathOutsideRoot {
				t.Fatalf("OpenParent(\"\"): %v", err)
			}

			out, err := os.Open(filepath.Dir(rootDir))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := r.Encloses(out); err != nil || ok {
				t.Fatalf("Encloses(parent of root) = %v %v", ok, err)
			}
			_ = out.Close()
		})
	}
}

// TestResolveMountCrossing opens "/" as a root without a floor and resolves
// into a different mount: /proc on Linux, /dev (devfs) on macOS.
func TestResolveMountCrossing(t *testing.T) {
	target := "proc/self/status"
	if runtime.GOOS == "darwin" {
		target = "dev/null"
	}
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			r, err := OpenDirRoot("/", RootOptions{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if r.Display() != "/" {
				t.Errorf("display %q", r.Display())
			}
			_, err = r.Open(target, unix.O_RDONLY|unix.O_NONBLOCK, 0)
			if codeOf(err) != proto.CodeFileTypeRefused || skipOf(err) != proto.SkipMount {
				t.Fatalf("open %s: %v, want a mount refusal", target, err)
			}
		})
	}
}

// TestRootPinned: the root handle stays on the first folder when the folder
// is renamed and another one takes its path (the pane's "cd" after admission
// changes nothing for the admitted transfer either).
func TestRootPinned(t *testing.T) {
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			base := tempDir(t)
			dir := filepath.Join(base, "app")
			mustWrite(t, filepath.Join(dir, "f"), "first")
			pid, stop := startPane(t, dir)
			r, err := OpenPaneRoot(pid, dir, RootOptions{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			stop()
			if err := os.Rename(dir, filepath.Join(base, "moved")); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(dir, "f"), "second")
			f, err := r.Open("f", unix.O_RDONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			_ = f.Close()
			if string(b) != "first" {
				t.Fatalf("read %q after rename, want the first folder", b)
			}
			// A new pane in the new folder gets the new folder.
			pid2, stop2 := startPane(t, dir)
			defer stop2()
			r2, err := OpenPaneRoot(pid2, dir, RootOptions{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r2.Close() }()
			if r2.Identity() == r.Identity() {
				t.Fatal("the new folder has the identity of the first")
			}
		})
	}
}

// startPane starts a process in dir standing in for a pane's shell.
func startPane(t *testing.T, dir string) (int, func()) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a pane process: %v", err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	return cmd.Process.Pid, stop
}

func TestPaneRootSymlinkInPath(t *testing.T) {
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			base := tempDir(t)
			realDir := filepath.Join(base, "real")
			mustMkdir(t, realDir)
			link := filepath.Join(base, "link")
			mustSymlink(t, realDir, link)
			pid, _ := startPane(t, realDir)
			if _, err := OpenPaneRoot(pid, link, RootOptions{Mode: mode}); codeOf(err) != proto.CodeFilesRootUnknown {
				t.Fatalf("pane path through a symlink: %v", err)
			}
			if _, err := OpenPaneRoot(pid, filepath.Join(link, "."), RootOptions{Mode: mode}); codeOf(err) != proto.CodeFilesRootUnknown {
				t.Fatalf("unclean path: %v", err)
			}
			if _, err := OpenPaneRoot(pid, "relative/path", RootOptions{Mode: mode}); codeOf(err) != proto.CodeFilesRootUnknown {
				t.Fatalf("relative path: %v", err)
			}
			if _, err := OpenDirRoot(link, RootOptions{Mode: mode}); codeOf(err) != proto.CodeFilesRootUnknown {
				t.Fatalf("dir root through a symlink: %v", err)
			}
		})
	}
}

// homeTree is a fake home for the floor tests.
type homeTree struct {
	home, state, app, hooks, ssh, other string
}

func makeHome(t *testing.T) homeTree {
	t.Helper()
	base := tempDir(t)
	h := homeTree{home: filepath.Join(base, "home")}
	h.state = filepath.Join(h.home, ".local", "state", "minion")
	h.app = filepath.Join(h.home, "projects", "app")
	h.hooks = filepath.Join(h.app, ".git", "hooks")
	h.ssh = filepath.Join(h.home, ".ssh")
	h.other = filepath.Join(base, "elsewhere")
	for _, d := range []string{h.state, h.hooks, filepath.Join(h.ssh, "sub"), filepath.Join(h.home, ".aws"),
		filepath.Join(h.other, ".aws", "x"), filepath.Join(h.app, ".claude"), filepath.Join(h.other, ".github", "workflows")} {
		mustMkdir(t, d)
	}
	return h
}

func TestFloorTable(t *testing.T) {
	etc, err := filepath.EvalSymlinks("/etc")
	if err != nil {
		t.Fatal(err)
	}
	h := makeHome(t)
	floor := NewFloor(FloorConfig{Home: h.home, StateDir: h.state})
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			cases := []struct {
				dir       string
				code      proto.Code
				protected bool
				display   string
			}{
				{"/", proto.CodeFilesRootRefused, false, ""},
				{etc, proto.CodeFilesRootRefused, false, ""},
				{h.home, proto.CodeFilesRootRefused, false, ""},
				{h.ssh, proto.CodeFilesRootRefused, false, ""},
				{filepath.Join(h.ssh, "sub"), proto.CodeFilesRootRefused, false, ""},
				{filepath.Join(h.home, ".aws"), proto.CodeFilesRootRefused, false, ""},
				{filepath.Join(h.other, ".aws", "x"), proto.CodeFilesRootRefused, false, ""},
				{h.state, proto.CodeFilesRootRefused, false, ""},
				{filepath.Dir(h.state), proto.CodeFilesRootRefused, false, ""},
				{h.app, "", false, "~/projects/app"},
				{h.hooks, "", true, "~/projects/app/.git/hooks"},
				{filepath.Join(h.app, ".claude"), "", true, "~/projects/app/.claude"},
				{filepath.Join(h.other, ".github", "workflows"), "", true, ""},
			}
			for _, c := range cases {
				r, err := OpenDirRoot(c.dir, RootOptions{Mode: mode, Floor: floor})
				if c.code != "" {
					if codeOf(err) != c.code {
						t.Errorf("%s: %v, want %s", c.dir, err, c.code)
					}
					if r != nil {
						_ = r.Close()
					}
					continue
				}
				if err != nil {
					t.Errorf("%s: %v", c.dir, err)
					continue
				}
				if r.PushProtected() != c.protected {
					t.Errorf("%s: push-protected %v, want %v", c.dir, r.PushProtected(), c.protected)
				}
				if c.protected && codeOf(r.CheckPush()) != proto.CodePathProtected {
					t.Errorf("%s: CheckPush %v", c.dir, r.CheckPush())
				}
				if c.display != "" && r.Display() != c.display {
					t.Errorf("%s: display %q, want %q", c.dir, r.Display(), c.display)
				}
				if strings.Contains(r.Display(), h.home) {
					t.Errorf("display %q names the home folder", r.Display())
				}
				_ = r.Close()
			}

			// Home is allowed with file_transfer_allow_home; the protected
			// lists relax with file_transfer_protected=off, the sensitive
			// ones do not.
			relaxed := NewFloor(FloorConfig{Home: h.home, StateDir: h.state, AllowHome: true, ProtectedOff: true})
			r, err := OpenDirRoot(h.home, RootOptions{Mode: mode, Floor: relaxed})
			if err != nil {
				t.Fatalf("home with allow_home: %v", err)
			}
			if r.Display() != "~" {
				t.Errorf("home display %q", r.Display())
			}
			_ = r.Close()
			r, err = OpenDirRoot(h.hooks, RootOptions{Mode: mode, Floor: relaxed})
			if err != nil || r.PushProtected() {
				t.Fatalf("hooks with protected=off: %v", err)
			}
			_ = r.Close()
			if _, err := OpenDirRoot(h.ssh, RootOptions{Mode: mode, Floor: relaxed}); codeOf(err) != proto.CodeFilesRootRefused {
				t.Fatalf(".ssh with protected=off: %v", err)
			}
		})
	}
}

func TestFloorIdentitySets(t *testing.T) {
	h := makeHome(t)
	floor := NewFloor(FloorConfig{Home: h.home, StateDir: h.state})
	ssh := statIdentity(h.ssh)
	state := statIdentity(h.state)
	app := statIdentity(h.app)
	if !floor.SensitiveDir(ssh) || !floor.SensitiveDir(state) || floor.SensitiveDir(app) {
		t.Fatal("SensitiveDir")
	}
	if !floor.ProtectedDir(ssh) || !floor.ProtectedDir(state) || floor.ProtectedDir(app) {
		t.Fatal("ProtectedDir")
	}
	off := NewFloor(FloorConfig{Home: h.home, StateDir: h.state, ProtectedOff: true})
	if !off.ProtectedDir(state) || !off.ProtectedOff() {
		t.Fatal("protected=off must keep sensitive folders protected")
	}
	var nilFloor *Floor
	if nilFloor.SensitiveDir(ssh) || nilFloor.ProtectedDir(ssh) {
		t.Fatal("nil floor")
	}
}

// TestCwdRoot: the REST CLI's own folder (FR-4).
func TestCwdRoot(t *testing.T) {
	h := makeHome(t)
	floor := NewFloor(FloorConfig{Home: h.home, StateDir: h.state})
	mode := testModes(t)[0]
	t.Chdir(h.home)
	if _, err := OpenCwdRoot(RootOptions{Mode: mode, Floor: floor}); codeOf(err) != proto.CodeFilesRootRefused {
		t.Fatalf("cwd home: %v", err)
	}
	t.Chdir(h.app)
	r, err := OpenCwdRoot(RootOptions{Mode: mode, Floor: floor})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if r.Display() != "~/projects/app" {
		t.Fatalf("display %q", r.Display())
	}
}

// TestConfine: the Android rule (BRD 5.4).
func TestConfine(t *testing.T) {
	h := makeHome(t)
	files := filepath.Join(h.home, "Minion files")
	sub := filepath.Join(files, "inbox")
	mustMkdir(t, sub)
	floor := NewFloor(FloorConfig{Home: h.home, StateDir: h.state})
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			// The configured Confine path may go through a system symlink.
			alias := filepath.Join(tempDir(t), "alias")
			mustSymlink(t, h.home, alias)
			opts := RootOptions{Mode: mode, Floor: floor, Confine: filepath.Join(alias, "Minion files"), ConfineLabel: "Minion files"}
			for _, c := range []struct{ pane, display string }{
				{h.home, "Minion files"},
				{h.app, "Minion files"},
				{files, "Minion files"},
				{sub, "Minion files/inbox"},
			} {
				pid, stop := startPane(t, c.pane)
				r, err := OpenPaneRoot(pid, c.pane, opts)
				stop()
				if err != nil {
					t.Errorf("pane in %s: %v", c.pane, err)
					continue
				}
				if r.Display() != c.display {
					t.Errorf("pane in %s: display %q, want %q", c.pane, r.Display(), c.display)
				}
				_ = r.Close()
			}
		})
	}
}

func TestUnprobedDefault(t *testing.T) {
	old := DefaultResolveMode()
	defer SetDefaultResolveMode(old)
	SetDefaultResolveMode(ResolveDefault)
	_, err := OpenDirRoot(tempDir(t), RootOptions{})
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("macOS walks without a probe: %v", err)
		}
		return
	}
	if codeOf(err) != proto.CodeFilesRootUnknown {
		t.Fatalf("Linux before the probe: %v", err)
	}
	SetDefaultResolveMode(ResolveWalk)
	r, err := OpenDirRoot(tempDir(t), RootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if _, err := r.Open("x", unix.O_RDONLY, 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("open after close: %v", err)
	}
}

// TestRaceTree is the fixed-input twin of FuzzResolve: random paths against a
// tree whose "race" entry flips between a folder and a symlink to the
// outside while the paths are opened.
func TestRaceTree(t *testing.T) {
	paths := []string{"race/f", "race", "race/deep/f", "a/b.txt", "inlink/b.txt", "race/../outside/secret", "abs"}
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			r, outside, stop := racingRoot(t, mode)
			defer stop()
			deadline := time.Now().Add(300 * time.Millisecond)
			for i := 0; time.Now().Before(deadline); i++ {
				checkNeverOutside(t, r, outside, paths[i%len(paths)])
			}
		})
	}
}

func FuzzResolve(f *testing.F) {
	for _, s := range []string{"race/f", "race/deep/f", "a/b.txt", "../outside/secret", "abs", "inlink/b.txt", "a/up/outside/secret", ""} {
		f.Add(s)
	}
	mode := ResolveWalk
	if runtime.GOOS == "linux" && openat2Works() {
		mode = ResolveOpenat2
	}
	r, outside, stop := racingRootF(f, mode)
	defer stop()
	f.Fuzz(func(t *testing.T, p string) {
		checkNeverOutside(t, r, outside, p)
	})
}

// checkNeverOutside opens p and fails if the opened entry is outside the root.
func checkNeverOutside(t *testing.T, r *Root, outside, p string) {
	t.Helper()
	for _, flags := range []int{unix.O_RDONLY | unix.O_NONBLOCK, unix.O_RDONLY | unix.O_DIRECTORY} {
		f, err := r.Open(p, flags, 0)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err == nil {
			if fi.IsDir() {
				if ok, err := r.Encloses(f); err != nil || !ok {
					t.Errorf("open %q: a folder outside the root (%v)", p, err)
				}
			} else {
				b, _ := io.ReadAll(io.LimitReader(f, 64))
				if string(b) == "outside" {
					t.Errorf("open %q: read the outside file", p)
				}
			}
		}
		_ = f.Close()
	}
	_ = outside
}

type fataler interface {
	Helper()
	Fatal(args ...any)
	TempDir() string
}

func racingRoot(t *testing.T, mode ResolveMode) (*Root, string, func()) {
	return racingRootF(t, mode)
}

// racingRootF builds sampleTree plus race/ (a folder with f and deep/f) that a
// goroutine swaps with a symlink to the outside folder, until stop.
func racingRootF(tb fataler, mode ResolveMode) (*Root, string, func()) {
	tb.Helper()
	base, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, p := range []string{filepath.Join(root, "a"), filepath.Join(root, "race", "deep"), filepath.Join(outside, "deep")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	for p, c := range map[string]string{
		filepath.Join(root, "a", "b.txt"): "inside", filepath.Join(root, "race", "f"): "inside",
		filepath.Join(root, "race", "deep", "f"): "inside", filepath.Join(outside, "f"): "outside",
		filepath.Join(outside, "secret"): "outside", filepath.Join(outside, "deep", "f"): "outside",
	} {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		filepath.Join(root, "a", "up"): "../..", filepath.Join(root, "inlink"): "a",
		filepath.Join(root, "abs"): filepath.Join(outside, "secret"), filepath.Join(root, "racelink"): outside,
	} {
		if err := os.Symlink(target, link); err != nil {
			tb.Fatal(err)
		}
	}
	r, err := OpenDirRoot(root, RootOptions{Mode: mode})
	if err != nil {
		tb.Fatal(err)
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		race, link, tmp := filepath.Join(root, "race"), filepath.Join(root, "racelink"), filepath.Join(root, "tmp")
		for {
			select {
			case <-done:
				return
			default:
			}
			// race <-> racelink: the folder and the outside symlink swap names.
			_ = os.Rename(race, tmp)
			_ = os.Rename(link, race)
			_ = os.Rename(tmp, link)
		}
	}()
	return r, outside, func() {
		close(done)
		<-finished
		_ = r.Close()
	}
}

func TestNameRules(t *testing.T) {
	split := func(p string) []string { return strings.Split(p, "/") }
	for _, c := range []struct {
		path                 string
		protected, sensitive bool
	}{
		{".git/hooks/pre-commit", true, false},
		{"x/.GIT/Hooks/pre-commit", true, false},
		{".git/config", true, false},
		{".git/HEAD", false, false},
		{".claude/settings.json", true, false},
		{".vscode/settings.json", true, false},
		{".vscode/launch.json", false, false},
		{".github/workflows/ci.yml", true, false},
		{".gitlab-ci.yml", true, false},
		{".env", true, false},
		{".env.local", true, false},
		{"sub/.envrc", true, false},
		{".bashrc", true, false},
		{"fish/config.fish", true, false},
		{"Makefile", false, false},
		{"src/main.go", false, false},
		{".ssh/id_ed25519", true, true},
		{".ssh/id_ed25519.pub", true, true},
		{"keys/id_rsa", false, true},
		{"keys/id_rsa.pub", false, false},
		{"tls/server.pem", false, true},
		{"tls/server.KEY", false, true},
		{".aws/credentials", false, true},
		{".netrc", true, true},
		{".git-credentials", false, true},
		{".config/minion/state.json", false, true},
		{".gnupg/x", true, true},
	} {
		e := split(c.path)
		if got := ProtectedPath(e); got != c.protected {
			t.Errorf("ProtectedPath(%q) = %v", c.path, got)
		}
		if got := SensitivePath(e); got != c.sensitive {
			t.Errorf("SensitivePath(%q) = %v", c.path, got)
		}
	}
	if ProtectedDirPath(split("home/u/.env")) || !ProtectedDirPath(split("r/.git/hooks/x")) {
		t.Error("ProtectedDirPath")
	}
	if SensitiveDirPath(split("p/id_tools")) || !SensitiveDirPath(split("u/.ssh")) {
		t.Error("SensitiveDirPath")
	}
}
