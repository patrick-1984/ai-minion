//go:build unix

package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// shortDir returns a short private directory: unix socket paths are limited
// to about 100 bytes and t.TempDir paths can be longer.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestListenCreatesAPrivateSocketInAPrivateDirectory(t *testing.T) {
	path := filepath.Join(shortDir(t), "run", "minion", SocketName)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// A second OS user cannot connect: connect(2) on a unix socket needs write
	// permission on the socket file and search permission on every directory
	// of the path. Mode 0600 in a 0700 directory leaves both to the owner.
	if m := mode(t, path); m.Perm() != 0o600 || m&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v, want a socket with 0600", m)
	}
	if m := mode(t, filepath.Dir(path)); m.Perm() != 0o700 || !m.IsDir() {
		t.Fatalf("directory mode = %v, want 0700", m)
	}
	if m := mode(t, filepath.Dir(filepath.Dir(path))); m.Perm() != 0o700 {
		t.Fatalf("parent directory created by Listen has mode %v, want 0700", m)
	}
}

func TestListenTightensALooseDirectoryOfTheUser(t *testing.T) {
	dir := filepath.Join(shortDir(t), "minion")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(filepath.Join(dir, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if m := mode(t, dir); m.Perm() != 0o700 {
		t.Fatalf("directory mode = %v, want 0700", m)
	}
}

func TestListenRefusesADirectorySymlinkAndAFileInItsPlace(t *testing.T) {
	base := shortDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(link, SocketName)); err == nil {
		t.Fatal("Listen through a directory symlink succeeded")
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(file, SocketName)); err == nil {
		t.Fatal("Listen below a regular file succeeded")
	}
}

func TestListenRefusesRelativeAndTooLongPaths(t *testing.T) {
	if _, err := Listen("worker.sock"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path: %v", err)
	}
	long := filepath.Join(shortDir(t), strings.Repeat("d", 120), SocketName)
	if _, err := Listen(long); err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Fatalf("long path: %v", err)
	}
}

// makeStale leaves a socket file behind without a listener, the state a
// crashed Worker leaves.
func makeStale(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()
	if mode(t, path)&os.ModeSocket == 0 {
		t.Fatal("no stale socket was left")
	}
}

func TestListenRemovesAStaleSocketWhenNobodyAnswers(t *testing.T) {
	path := filepath.Join(shortDir(t), "minion", SocketName)
	makeStale(t, path)
	l, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	defer l.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the new socket does not accept: %v", err)
	}
	_ = c.Close()
}

func TestListenKeepsALiveSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "minion", SocketName)
	first, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	srv, _ := New(Config{Executor: echoExecutor()})
	go func() { _ = srv.Serve(first) }()
	defer srv.Close(context.Background())

	if _, err := Listen(path); err == nil || !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Listen = %v, want ErrAlreadyRunning", err)
	}
	// The first Worker still serves.
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the live socket was taken away: %v", err)
	}
	defer c.Close()
	if err := proto.WriteFrame(c, proto.StreamFrameLimit, request("sessions_list", nil)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, err := proto.ReadFrame(c, proto.StreamFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || string(m["type"]) != `"op.result"` {
		t.Fatalf("answer = %s", body)
	}
}

func TestListenNeverRemovesWhatIsNotASocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "minion", SocketName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Listen over a file = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "precious" {
		t.Fatal("the file was touched")
	}
}

func TestListenDoesNotRemoveASocketItCannotProbe(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can always connect")
	}
	path := filepath.Join(shortDir(t), "minion", SocketName)
	makeStale(t, path)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil {
		t.Fatal("Listen removed a socket it could not probe")
	}
	if mode(t, path)&os.ModeSocket == 0 {
		t.Fatal("the socket file is gone")
	}
}

func TestClosingTheListenerRemovesTheSocketFile(t *testing.T) {
	path := filepath.Join(shortDir(t), "minion", SocketName)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file after close: %v", err)
	}
}

func TestOwnedByMeRejectsAForeignOwner(t *testing.T) {
	fi, err := os.Lstat(shortDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByMe(fi) {
		t.Fatal("a directory we created is not ours")
	}
	st := fi.Sys().(*syscall.Stat_t)
	st.Uid++ // a copy owned by somebody else
	if ownedByMe(fi) {
		t.Fatal("a foreign owner was accepted")
	}
}

func TestConcurrentStartsOfTheSameSocketHaveExactlyOneWinner(t *testing.T) {
	for round := 0; round < 20; round++ {
		path := filepath.Join(shortDir(t), "minion", SocketName)
		makeStale(t, path) // every starter first sees a stale socket

		const starters = 8
		type result struct {
			l   net.Listener
			err error
		}
		results := make([]result, starters)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i].l, results[i].err = Listen(path)
			}()
		}
		close(start)
		wg.Wait()

		var winners []net.Listener
		for _, r := range results {
			switch {
			case r.err == nil:
				winners = append(winners, r.l)
			case !errors.Is(r.err, ErrAlreadyRunning):
				t.Fatalf("round %d: a loser failed with %v, want ErrAlreadyRunning", round, r.err)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("round %d: %d Listen calls succeeded, want exactly one", round, len(winners))
		}
		// The winner's socket is still the one at the path.
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatalf("round %d: the winner's socket does not answer: %v", round, err)
		}
		_ = c.Close()
		_ = winners[0].Close()
	}
}

func TestVerifySocketAcceptsAPrivateSocketOfTheUser(t *testing.T) {
	path := filepath.Join(shortDir(t), "minion", SocketName)
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := VerifySocket(path); err != nil {
		t.Fatalf("VerifySocket: %v", err)
	}
}

func TestVerifySocketReportsAMissingPathAsNotExist(t *testing.T) {
	dir := shortDir(t)
	for _, p := range []string{filepath.Join(dir, SocketName), filepath.Join(dir, "no", "dir", SocketName)} {
		if err := VerifySocket(p); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrUntrusted) {
			t.Fatalf("VerifySocket(%s) = %v, want not-exist", p, err)
		}
	}
}

func TestVerifySocketRefusesADirectoryOpenToGroupOrOthers(t *testing.T) {
	for _, m := range []os.FileMode{0o755, 0o750, 0o705, 0o770} {
		t.Run(fmt.Sprintf("%o", m), func(t *testing.T) {
			dir := filepath.Join(shortDir(t), "minion")
			path := filepath.Join(dir, SocketName)
			makeStale(t, path)
			if err := os.Chmod(dir, m); err != nil {
				t.Fatal(err)
			}
			if err := VerifySocket(path); !errors.Is(err, ErrUntrusted) {
				t.Fatalf("VerifySocket in a %o directory = %v, want ErrUntrusted", m, err)
			}
		})
	}
}

func TestVerifySocketRefusesAForeignOwner(t *testing.T) {
	dir := filepath.Join(shortDir(t), "minion")
	path := filepath.Join(dir, SocketName)
	makeStale(t, path)
	foreign := func(target string) func(os.FileInfo) bool {
		return func(fi os.FileInfo) bool {
			if fi.Name() != filepath.Base(target) {
				return ownedByMe(fi)
			}
			st := *fi.Sys().(*syscall.Stat_t) // a copy owned by somebody else
			st.Uid++
			return int(st.Uid) == os.Getuid()
		}
	}
	if err := verifySocket(path, foreign(path)); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a socket of another user: %v, want ErrUntrusted", err)
	}
	if err := verifySocket(path, foreign(dir)); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a directory of another user: %v, want ErrUntrusted", err)
	}
	if err := verifySocket(path, ownedByMe); err != nil {
		t.Fatalf("control: %v", err)
	}
}

func TestVerifySocketRefusesSymlinksAndNonSockets(t *testing.T) {
	base := shortDir(t)
	real := filepath.Join(base, "real")
	makeStale(t, filepath.Join(real, SocketName))
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := VerifySocket(filepath.Join(link, SocketName)); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a directory symlink: %v, want ErrUntrusted", err)
	}
	sockLink := filepath.Join(real, "alias.sock")
	if err := os.Symlink(filepath.Join(real, SocketName), sockLink); err != nil {
		t.Fatal(err)
	}
	if err := VerifySocket(sockLink); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a socket symlink: %v, want ErrUntrusted", err)
	}
	file := filepath.Join(real, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySocket(file); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("a regular file: %v, want ErrUntrusted", err)
	}
}
