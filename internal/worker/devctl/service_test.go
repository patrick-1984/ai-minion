package devctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/devctl"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl/devctltest"
)

const token = "tok-0123456789abcdef"

type fixture struct {
	fake  *devctltest.Bridge
	svc   *devctl.Service
	audit string

	mu    sync.Mutex
	share string
}

func (f *fixture) setShare(m string) { f.mu.Lock(); f.share = m; f.mu.Unlock() }

func newFixture(t *testing.T, goos string) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "dc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fake, err := devctltest.Start(dir, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	f := &fixture{fake: fake, share: devctl.ShareNone, audit: filepath.Join(dir, "adb", "audit.log")}
	f.svc = devctl.New(devctl.Config{
		GOOS:   goos,
		Bridge: devctl.NewClient(fake.Path, func() string { return token }),
		ShareMode: func(context.Context) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.share, nil
		},
		// Pane pid 100 is session "build"; the caller 300 is a child of 200, a
		// child of 100.
		Panes: func(context.Context) (map[int]string, error) { return map[int]string{100: "build"}, nil },
		PPID: func(pid int) (int, error) {
			return map[int]int{300: 200, 200: 100, 100: 1}[pid], nil
		},
		Audit: devctl.NewAuditLog(filepath.Join(dir, "adb", "audit.log"), nil),
	})
	return f
}

func caller(pid int) context.Context {
	return devctl.WithCaller(context.Background(), devctl.Caller{PID: pid, Actor: "alice"})
}

func TestShellRelaysOutputAndExitCode(t *testing.T) {
	f := newFixture(t, "android")
	f.fake.Handle(func(op, cmd string) devctltest.Result {
		if op != devctl.OpShell || cmd != "id -u" {
			return devctltest.Result{Code: 99}
		}
		return devctltest.Result{Stdout: []byte("2000\n"), Stderr: []byte("warn"), Code: 7}
	})
	var out, errb strings.Builder
	code, err := f.svc.Run(caller(300), devctl.OpShell, "id -u", time.Second, func(stderr bool, b []byte) error {
		if stderr {
			errb.Write(b)
		} else {
			out.Write(b)
		}
		return nil
	})
	if err != nil || code != 7 || out.String() != "2000\n" || errb.String() != "warn" {
		t.Fatalf("code %d err %v out %q err %q", code, err, out.String(), errb.String())
	}
	rec := f.svc.Recent()
	if len(rec) != 1 || rec[0].Op != "shell" || rec[0].Result != "exit:7" || rec[0].Session != "build" || rec[0].Actor != "alice" || rec[0].Transport != "tls-loopback" {
		t.Fatalf("audit %+v", rec)
	}
	if r := f.fake.Requests(); len(r) != 2 || r[1].Token != token || r[1].TimeoutMS != 1000 {
		t.Fatalf("requests %+v", r)
	}
}

func TestLargeScreencapStreamsInChunks(t *testing.T) {
	f := newFixture(t, "android")
	png := make([]byte, 3*devctl.MaxPayload+17)
	for i := range png {
		png[i] = byte(i)
	}
	f.fake.Handle(func(string, string) devctltest.Result { return devctltest.Result{Stdout: png} })
	var got []byte
	code, err := f.svc.Run(caller(1), devctl.OpScreencap, "", 0, func(_ bool, b []byte) error { got = append(got, b...); return nil })
	if err != nil || code != 0 || string(got) != string(png) {
		t.Fatalf("code %d err %v len %d", code, err, len(got))
	}
	if rec := f.svc.Recent(); rec[0].Session != "unknown" || rec[0].Result != "ok" {
		t.Fatalf("audit %+v", rec)
	}
}

func TestPolicyRefusals(t *testing.T) {
	cases := []struct {
		name   string
		goos   string
		status *devctl.Status
		share  string
		code   string
		result string
	}{
		{"not android", "linux", nil, "none", devctl.ErrUnsupportedOS, "denied:unsupported_platform"},
		{"build flag off", "android", &devctl.Status{FeatureEnabled: false, Enabled: true, State: "READY"}, "none", devctl.ErrNotAvailable, "denied:not_available"},
		{"not enabled", "android", &devctl.Status{FeatureEnabled: true, State: "OFF"}, "none", devctl.ErrDisabled, "denied:disabled"},
		{"shared", "android", nil, "all", devctl.ErrShared, "denied:shared"},
		{"later counts as not none", "android", nil, "later", devctl.ErrShared, "denied:shared"},
		{"no wifi", "android", &devctl.Status{FeatureEnabled: true, Enabled: true, State: "WAITING_WIFI"}, "none", devctl.ErrNeedsWiFi, "error:needs_wifi"},
		{"provenance", "android", &devctl.Status{FeatureEnabled: true, Enabled: true, State: "ENDPOINT_UNVERIFIED"}, "none", devctl.ErrEndpointUnverified, "denied:provenance"},
		{"unpaired", "android", &devctl.Status{FeatureEnabled: true, Enabled: true, State: "UNPAIRED"}, "none", devctl.ErrNotPaired, "error:not_paired"},
		{"tv dialog open", "android", &devctl.Status{FeatureEnabled: true, Enabled: true, State: "AWAITING_AUTHORIZATION", Transport: "legacy-loopback"}, "none", devctl.ErrNeedsAuth, "error:needs_authorization"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, c.goos)
			if c.status != nil {
				f.fake.SetStatus(*c.status)
			}
			f.setShare(c.share)
			_, err := f.svc.Run(caller(1), devctl.OpShell, "id", 0, nil)
			if devctl.CodeOf(err) != c.code {
				t.Fatalf("err %v, want code %s", err, c.code)
			}
			for _, op := range f.fake.Ops() {
				if op == devctl.OpShell {
					t.Fatal("a refused request reached the bridge as a shell")
				}
			}
			if rec := f.svc.Recent(); len(rec) != 1 || rec[0].Result != c.result {
				t.Fatalf("audit %+v", rec)
			}
		})
	}
}

// The legacy transport (adb on 127.0.0.1:5555) is reported and audited as
// such; the first connection tells the owner to confirm the dialog.
func TestLegacyTransportReportedAndAudited(t *testing.T) {
	f := newFixture(t, "android")
	f.fake.SetStatus(devctl.Status{FeatureEnabled: true, Enabled: true, State: devctl.StateAwaitingAuth, Transport: devctl.TransportLegacyLoopback})
	r := f.svc.Report(context.Background())
	if r.State != devctl.StateAwaitingAuth || r.Transport != devctl.TransportLegacyLoopback || !strings.Contains(r.Reason, "Allow debugging?") {
		t.Fatalf("report %+v", r)
	}
	f.fake.SetStatus(devctl.Status{FeatureEnabled: true, Enabled: true, State: devctl.StateReady, Port: 5555, Transport: devctl.TransportLegacyLoopback})
	if _, err := f.svc.Run(caller(1), devctl.OpShell, "id", 0, nil); err != nil {
		t.Fatal(err)
	}
	if r := f.svc.Report(context.Background()); r.Transport != devctl.TransportLegacyLoopback || r.Port != 5555 || r.Reason != "" {
		t.Fatalf("report %+v", r)
	}
	rec := f.svc.Recent()
	if len(rec) != 1 || rec[0].Op != "shell" || rec[0].Transport != devctl.TransportLegacyLoopback {
		t.Fatalf("audit %+v", rec)
	}
}

// PAIRED (not connected now) goes to the bridge, which connects on demand.
func TestPairedGoesToTheBridge(t *testing.T) {
	f := newFixture(t, "android")
	f.fake.SetStatus(devctl.Status{FeatureEnabled: true, Enabled: true, State: devctl.StatePaired})
	if _, err := f.svc.Run(caller(1), devctl.OpShell, "id", 0, nil); err != nil {
		t.Fatal(err)
	}
	f.fake.SetStatus(devctl.Status{FeatureEnabled: false, State: devctl.StateUnavailable})
	if _, err := f.svc.Run(caller(1), devctl.OpShell, "id", 0, nil); devctl.CodeOf(err) != devctl.ErrNotAvailable {
		t.Fatal(err)
	}
}

func TestSharedWorkerDisablesDeviceControl(t *testing.T) {
	f := newFixture(t, "android")
	f.setShare("all")
	_, _ = f.svc.Run(caller(1), devctl.OpShell, "id", 0, nil)
	if f.fake.Status().Enabled {
		t.Fatal("a shared Worker left device control enabled")
	}
}

func TestDisableClosesStreams(t *testing.T) {
	f := newFixture(t, "android")
	f.fake.Handle(func(string, string) devctltest.Result { return devctltest.Result{Block: true} })
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Run(caller(1), devctl.OpShell, "sleep 600", time.Hour, nil)
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.fake.WaitActive(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if r := f.svc.Report(context.Background()); r.ActiveStreams != 1 {
		t.Fatalf("active %d", r.ActiveStreams)
	}
	start := time.Now()
	if err := f.svc.Disable(context.Background(), devctl.ReasonHub); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if devctl.CodeOf(err) != devctl.ErrDisabled {
			t.Fatalf("stream ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not end within 2 s of disable")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("too slow")
	}
	if _, err := f.svc.Run(caller(1), devctl.OpShell, "id", 0, nil); devctl.CodeOf(err) != devctl.ErrDisabled {
		t.Fatalf("new call after disable: %v", err)
	}
}

func TestShareChangeDisables(t *testing.T) {
	f := newFixture(t, "android")
	if err := f.svc.GuardShare(context.Background(), "all"); !errors.Is(err, devctl.ErrShareRefused) {
		t.Fatalf("guard: %v", err)
	}
	if err := f.svc.GuardShare(context.Background(), "none"); err != nil {
		t.Fatalf("guard none: %v", err)
	}
	f.svc.ShareChanged(context.Background(), "select")
	if f.fake.Status().Enabled {
		t.Fatal("a share change left device control enabled")
	}
	if err := f.svc.GuardShare(context.Background(), "all"); err != nil {
		t.Fatalf("guard after disable: %v", err)
	}
}

func TestWatchDisablesWhenSharedBehindItsBack(t *testing.T) {
	f := newFixture(t, "android")
	f.setShare("all")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.svc.Watch(ctx, 10*time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for f.fake.Status().Enabled {
		if time.Now().After(deadline) {
			t.Fatal("watch did not disable")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTimeout(t *testing.T) {
	f := newFixture(t, "android")
	f.fake.Handle(func(string, string) devctltest.Result { return devctltest.Result{Block: true} })
	start := time.Now()
	_, err := f.svc.Run(caller(1), devctl.OpShell, "sleep 10", time.Second, nil)
	if devctl.CodeOf(err) != devctl.ErrTimeout {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeout took %s", d)
	}
}

func TestBridgeDown(t *testing.T) {
	svc := devctl.New(devctl.Config{GOOS: "android", Bridge: devctl.NewClient("/nonexistent/adb-bridge.sock", nil),
		ShareMode: func(context.Context) (string, error) { return "none", nil }})
	start := time.Now()
	_, err := svc.Run(context.Background(), devctl.OpShell, "id", 0, nil)
	if devctl.CodeOf(err) != devctl.ErrBridgeDown || time.Since(start) > 6*time.Second {
		t.Fatalf("err %v", err)
	}
	r := svc.Report(context.Background())
	if r.Available || r.Enabled || r.State != "OFF" || r.Reason == "" {
		t.Fatalf("report %+v", r)
	}
}

func TestBadToken(t *testing.T) {
	f := newFixture(t, "android")
	svc := devctl.New(devctl.Config{GOOS: "android", Bridge: devctl.NewClient(f.fake.Path, func() string { return "wrong" }),
		ShareMode: func(context.Context) (string, error) { return "none", nil }})
	if _, err := svc.Run(context.Background(), devctl.OpShell, "id", 0, nil); devctl.CodeOf(err) != devctl.ErrBadToken {
		t.Fatalf("err %v", err)
	}
}

// The audit file holds the field set of DESIGN 4.8 and never a command,
// output, code, key or token.
func TestAuditRedaction(t *testing.T) {
	f := newFixture(t, "android")
	secret := "SECRET-CMD-4711"
	f.fake.Handle(func(string, string) devctltest.Result {
		return devctltest.Result{Stdout: []byte("SECRET-OUT-0815"), Stderr: []byte("SECRET-ERR")}
	})
	_, _ = f.svc.Run(caller(300), devctl.OpShell, "echo "+secret, 0, nil)
	_ = f.svc.Disable(context.Background(), devctl.ReasonLocal)
	_, _ = f.svc.Run(caller(300), devctl.OpInput, "input text "+secret, 0, nil)
	b, err := os.ReadFile(f.audit)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{secret, "SECRET-OUT", "SECRET-ERR", token, "echo", "input text"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("audit holds %q:\n%s", bad, b)
		}
	}
	fi, _ := os.Stat(f.audit)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("audit mode %v", fi.Mode())
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines %d", len(lines))
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		if len(m) != 7 {
			t.Fatalf("fields %v", m)
		}
		for _, k := range []string{"time", "actor", "session", "op", "transport", "result", "duration_ms"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("missing %s in %v", k, m)
			}
		}
	}
}

func TestAuditRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")
	a := devctl.NewAuditLog(p, nil)
	for range 30000 {
		if err := a.Record(devctl.Entry{Op: "status", Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"", ".1", ".2", ".3"} {
		fi, err := os.Stat(p + suffix)
		if err != nil {
			t.Fatalf("%s: %v", suffix, err)
		}
		if fi.Size() > devctl.AuditMaxBytes {
			t.Fatalf("%s is %d bytes", suffix, fi.Size())
		}
	}
	if _, err := os.Stat(p + ".4"); err == nil {
		t.Fatal("more than 3 old files kept")
	}
	if len(a.Recent()) != devctl.AuditRecent {
		t.Fatal("recent")
	}
}

func TestMapSession(t *testing.T) {
	pp := map[int]int{50: 40, 40: 30, 30: 1, 60: 60}
	ppid := func(p int) (int, error) {
		if v, ok := pp[p]; ok {
			return v, nil
		}
		return 0, errors.New("gone")
	}
	panes := map[int]string{30: "docs"}
	if s := devctl.MapSession(50, ppid, panes); s != "docs" {
		t.Fatal(s)
	}
	if s := devctl.MapSession(60, ppid, panes); s != "unknown" {
		t.Fatal("loop", s)
	}
	if s := devctl.MapSession(99, ppid, panes); s != "unknown" {
		t.Fatal(s)
	}
}
