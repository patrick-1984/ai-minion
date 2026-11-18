package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// otlpSink is an OTLP/HTTP endpoint that checks the credential of every
// request against the one it was configured with.
type otlpSink struct {
	srv      *httptest.Server
	want     string
	requests atomic.Int64
	wrong    atomic.Int64
}

func newOTLPSink(t *testing.T, want string) *otlpSink {
	s := &otlpSink{want: want}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.requests.Add(1)
		if r.Header.Get("X-Key") != s.want || r.URL.Path != "/v1/logs" {
			s.wrong.Add(1)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(s.srv.Close)
	return s
}

type obsFixture struct {
	*fixture
	settings *SettingsService
	svc      *ObsService
	log      *slog.Logger
	local    *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newObsFixture wires the settings service (default registry, real store and
// audit) to a Hub logger and its sink manager, as the logging module does.
func newObsFixture(t *testing.T, env map[string]string, verbose *bool) *obsFixture {
	t.Helper()
	f := newFixture(t)
	for k, v := range env {
		f.env[k] = v
	}
	settings, err := NewSettingsService(context.Background(), SettingsDeps{
		Store: f.st, Auditor: settingsAuditAdapter{log: audit.New(f.st)}, LookupEnv: f.env.lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	local := &syncBuffer{}
	log, live, err := logx.NewLiveLocal(logx.Options{Level: "info", Format: logx.FormatText, Output: local})
	if err != nil {
		t.Fatal(err)
	}
	m := obs.New(live.Fanout, obs.Process{Service: "minion-hub"})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	svc := &ObsService{Manager: m, live: live, verbose: verbose}
	if err := wireLogging(context.Background(), svc, settings, config.SinkFiles{}); err != nil {
		t.Fatal(err)
	}
	return &obsFixture{fixture: f, settings: settings, svc: svc, log: log, local: local}
}

func (o *obsFixture) set(t *testing.T, changes ...ops.SettingChange) ops.WriteResult {
	t.Helper()
	res, err := o.settings.Set(context.Background(), admin, batchArgs(changes...))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Rotating endpoint and credential in one batch while logging under load: no
// request reaches the new endpoint with the old credential or the old
// endpoint with the new one, and the old queue is discarded (counted).
func TestRotateEndpointAndHeadersAtomically(t *testing.T) {
	a, b := newOTLPSink(t, "key-a"), newOTLPSink(t, "key-b")
	o := newObsFixture(t, nil, nil)
	o.set(t, ops.SettingChange{Key: "otel", Value: "on"}, ops.SettingChange{Key: "otel_endpoint", Value: a.srv.URL},
		ops.SettingChange{Key: "otel_headers", Value: "X-Key: key-a"})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					o.log.Info("load", "n", 1)
				}
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond)
	o.set(t, ops.SettingChange{Key: "otel_endpoint", Value: b.srv.URL}, ops.SettingChange{Key: "otel_headers", Value: "X-Key: key-b"})
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err := o.svc.Manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.requests.Load() == 0 || b.requests.Load() == 0 {
		t.Fatalf("requests a=%d b=%d", a.requests.Load(), b.requests.Load())
	}
	if a.wrong.Load() != 0 || b.wrong.Load() != 0 {
		t.Fatalf("mismatched credential: a=%d b=%d", a.wrong.Load(), b.wrong.Load())
	}
	st := o.svc.Manager.Status()[1]
	if st.Name != "otel" || st.Generation != 2 || st.Dropped["reconfigure"] == 0 {
		t.Fatalf("status = %+v", st)
	}
}

// A batch that would break a sink is refused before anything is stored.
func TestInvalidSinkBatchIsRefused(t *testing.T) {
	o := newObsFixture(t, nil, nil)
	_, err := o.settings.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: "syslog", Value: "on"}))
	if err == nil || !strings.Contains(err.Error(), "syslog_endpoint") {
		t.Fatalf("err = %v", err)
	}
	if recs, _ := o.st.ListSettings(context.Background()); len(recs) != 0 {
		t.Fatalf("stored: %+v", recs)
	}
	if st := o.svc.Manager.Status(); st[0].Enabled || st[0].Generation != 0 {
		t.Fatalf("status = %+v", st)
	}
}

// The status names the destination without credentials and counts sends.
func TestSinkStatusAndSyslogApply(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 100)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 65536)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	o := newObsFixture(t, nil, nil)
	o.set(t, ops.SettingChange{Key: "syslog", Value: "on"}, ops.SettingChange{Key: "syslog_endpoint", Value: "tcp://" + ln.Addr().String()},
		ops.SettingChange{Key: "syslog_level", Value: "warn"})
	o.log.Info("not sent")
	o.log.Warn("sent", "component", "test")
	select {
	case m := <-got:
		if !strings.Contains(m, "minion-hub") || !strings.Contains(m, `"msg":"sent"`) || strings.Contains(m, "not sent") {
			t.Fatalf("frame = %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing received")
	}
	time.Sleep(50 * time.Millisecond)
	st := o.svc.Manager.Status()[0]
	if !st.Enabled || st.Destination != "tcp://"+ln.Addr().String() || st.Level != "warn" || st.Sent != 1 || st.LastSuccess == nil {
		t.Fatalf("status = %+v", st)
	}
	o.set(t, ops.SettingChange{Key: "syslog", Value: "off"})
	if st := o.svc.Manager.Status()[0]; st.Enabled || st.Generation != 2 {
		t.Fatalf("status after off = %+v", st)
	}
}

// Shutdown with dead endpoints ends within the obs share of the Hub budget.
func TestShutdownWithDeadEndpointsWithinBudget(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	_ = ln.Close()
	o := newObsFixture(t, nil, nil)
	o.set(t, ops.SettingChange{Key: "otel", Value: "on"}, ops.SettingChange{Key: "otel_endpoint", Value: "http://" + dead},
		ops.SettingChange{Key: "syslog", Value: "on"}, ops.SettingChange{Key: "syslog_endpoint", Value: "tcp://" + dead})
	for range 2000 {
		o.log.Info("x")
	}
	hub, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
	defer cancel()
	sctx, scancel := obs.StopContext(hub, obsStopLimit)
	defer scancel()
	start := time.Now()
	_ = o.svc.Manager.Shutdown(sctx)
	if d := time.Since(start); d > obsStopLimit+500*time.Millisecond {
		t.Fatalf("shutdown took %s", d)
	}
	if !strings.Contains(o.local.String(), "records were never sent") {
		t.Fatalf("no loss summary on the local log: %s", o.local.String())
	}
	// A nearly spent budget leaves half a second for what follows.
	short, c2 := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer c2()
	s2, c3 := obs.StopContext(short, obsStopLimit)
	defer c3()
	if dl, _ := s2.Deadline(); time.Until(dl) > 300*time.Millisecond {
		t.Fatalf("stop context ends in %s", time.Until(dl))
	}
}

// The panel's log_level applies live and keeps env > panel > -v > default.
func TestHubLogLevelIsLive(t *testing.T) {
	o := newObsFixture(t, nil, nil)
	lvl := func() slog.Level { return o.svc.live.Level.Level() }
	if lvl() != slog.LevelInfo {
		t.Fatalf("start level %v", lvl())
	}
	o.set(t, ops.SettingChange{Key: "log_level", Value: "debug"})
	if lvl() != slog.LevelDebug {
		t.Fatalf("panel debug not applied: %v", lvl())
	}
	o.set(t, ops.SettingChange{Key: "log_level", Unset: true})
	if lvl() != slog.LevelInfo {
		t.Fatalf("unset not applied: %v", lvl())
	}

	on := true
	v := newObsFixture(t, nil, &on)
	if v.svc.live.Level.Level() != slog.LevelDebug {
		t.Fatal("-v did not give debug")
	}
	v.set(t, ops.SettingChange{Key: "log_level", Value: "warn"})
	if v.svc.live.Level.Level() != slog.LevelWarn {
		t.Fatal("the panel did not win over -v")
	}

	e := newObsFixture(t, map[string]string{"MINION_LOG_LEVEL": "error"}, nil)
	if e.svc.live.Level.Level() != slog.LevelError {
		t.Fatal("env level not applied")
	}
	if _, err := e.settings.Set(context.Background(), admin, setArgs("log_level", "debug")); err == nil {
		t.Fatal("the panel changed an env-set level")
	}
	if e.svc.live.Level.Level() != slog.LevelError {
		t.Fatal("env level lost")
	}
}
