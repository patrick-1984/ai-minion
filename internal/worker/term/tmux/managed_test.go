package tmux

import (
	"context"
	"errors"
	"fmt"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

func recordingExec(calls *[][]string) ExecFunc {
	return func(_ context.Context, args []string, _ []byte) ([]byte, []byte, error) {
		*calls = append(*calls, args)
		if len(args) > 0 && args[len(args)-1] == "-V" {
			return []byte("tmux 3.8\n"), nil, nil
		}
		return nil, nil, nil
	}
}

// A managed host passes -N and the fixed configuration on every call,
// including the version check, so that no command starts a server.
func TestManagedHostPassesNoServerAndFixedConfig(t *testing.T) {
	skipWindows(t)
	var calls [][]string
	h, err := New("/data/run/tmux.sock", WithExec(recordingExec(&calls)), WithManaged("/data/etc/tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !h.Managed() {
		t.Fatal("not managed")
	}
	_, _ = h.run(context.Background(), "new-session", "-d")
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	for _, c := range calls {
		got := strings.Join(c, " ")
		if !strings.HasPrefix(got, "-S /data/run/tmux.sock -N -f /data/etc/tmux.conf -u ") {
			t.Errorf("args = %q", got)
		}
	}
}

func TestManagedHostDefaultsToAnEmptyConfig(t *testing.T) {
	skipWindows(t)
	var calls [][]string
	if _, err := New("/data/run/tmux.sock", WithExec(recordingExec(&calls)), WithManaged("")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls[0], " "); !strings.Contains(got, "-N -f "+DefaultManagedConfig+" ") {
		t.Errorf("args = %q", got)
	}
}

func TestOrdinaryHostHasNeitherFlag(t *testing.T) {
	skipWindows(t)
	var calls [][]string
	if _, err := New("/data/run/tmux.sock", WithExec(recordingExec(&calls))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls[0], " "); strings.Contains(got, "-N") || strings.Contains(got, "-f") {
		t.Errorf("args = %q", got)
	}
}

func TestOptionsForAppliesBinaryAndManagedMode(t *testing.T) {
	skipWindows(t)
	if n := len(OptionsFor(config.ManagedHost{})); n != 0 {
		t.Fatalf("the zero spec gave %d options", n)
	}
	var calls [][]string
	opts := append(OptionsFor(config.ManagedHost{Android: true, TmuxBinary: "/data/app/lib/arm64/libtmux.so", TmuxConfig: "/data/f/tmux.conf"}),
		WithExec(recordingExec(&calls)))
	h, err := New("/data/run/tmux.sock", opts...)
	if err != nil {
		t.Fatal(err)
	}
	if h.bin != "/data/app/lib/arm64/libtmux.so" || !h.managed || h.configFile != "/data/f/tmux.conf" {
		t.Errorf("host = %s", fmt.Sprintf("%+v", struct {
			bin  string
			m    bool
			conf string
		}{h.bin, h.managed, h.configFile}))
	}
	plain := OptionsFor(config.ManagedHost{TmuxBinary: "/opt/tmux"})
	h2, _ := New("/data/run/tmux.sock", append(plain, WithExec(recordingExec(&calls)))...)
	if h2.bin != "/opt/tmux" || h2.managed {
		t.Errorf("plain binary host: bin %q managed %v", h2.bin, h2.managed)
	}
}

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("slash socket paths are not absolute on Windows")
	}
}

func downExec(stderr string) ExecFunc {
	return func(_ context.Context, args []string, _ []byte) ([]byte, []byte, error) {
		if args[len(args)-1] == "-V" {
			return []byte("tmux 3.8\n"), nil, nil
		}
		return nil, []byte(stderr), fmt.Errorf("exit status 1")
	}
}

// With -N a missing managed server is an outage, never a healthy empty list,
// and a create during it is a definite failure (nothing was started).
func TestManagedServerOutageIsNotAnEmptyList(t *testing.T) {
	skipWindows(t)
	for _, msg := range []string{"no server running on /data/run/tmux.sock", "error connecting to /data/run/tmux.sock (No such file or directory)"} {
		h, err := New("/data/run/tmux.sock", WithExec(downExec(msg)), WithManaged(""))
		if err != nil {
			t.Fatal(err)
		}
		if ss, err := h.ListSessions(context.Background()); !errors.Is(err, term.ErrHostDown) || ss != nil {
			t.Errorf("%q: list = %v, %v", msg, ss, err)
		}
		if _, err := h.CreateSession(context.Background(), "minion-1", "", []string{"sh"}); !errors.Is(err, term.ErrHostDown) {
			t.Errorf("%q: create err = %v", msg, err)
		}
		// An ordinary host keeps treating a missing server as no sessions.
		o, _ := New("/data/run/tmux.sock", WithExec(downExec(msg)))
		if ss, err := o.ListSessions(context.Background()); err != nil || ss != nil {
			t.Errorf("ordinary host: %v, %v", ss, err)
		}
	}
}
