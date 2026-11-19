package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// startLogging wires a Hub logger the way the logging module does at start.
func startLogging(t *testing.T, f *fixture) (*ObsService, error) {
	t.Helper()
	settings, err := NewSettingsService(context.Background(), SettingsDeps{
		Store: f.st, Auditor: settingsAuditAdapter{log: audit.New(f.st)}, LookupEnv: f.env.lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, live, _ := logx.NewLiveLocal(logx.Options{Output: io.Discard})
	m := obs.New(live.Fanout, obs.Process{Service: "minion-hub"})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	svc := &ObsService{Manager: m, live: live}
	return svc, wireLogging(context.Background(), svc, settings, config.SinkFiles{})
}

// A configuration that no longer resolves at start leaves both sinks off
// (no environment-only fallback keeps sending) and never stops the Hub.
func TestStartupFailsClosedOnAnInvalidPanelConfiguration(t *testing.T) {
	f := newFixture(t)
	f.env["MINION_OTEL"] = "on"
	f.env[config.EnvOTLPEndpoint] = "http://127.0.0.1:1"
	ctx := context.Background()
	if err := f.st.PutSetting(ctx, "otel_ca_file", "/nonexistent/ca.pem", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutSetting(ctx, "otel_endpoint", "https://panel.example", false, "admin"); err != nil {
		t.Fatal(err)
	}
	svc, err := startLogging(t, f)
	if err != nil {
		t.Fatalf("a bad panel value stopped the start: %v", err)
	}
	st := svc.Manager.Status()
	if st[1].Enabled || svc.live.Fanout.Handler(logx.SlotOTel) != nil || !strings.Contains(st[1].LastError, "otel_ca_file") {
		t.Fatalf("status = %+v", st)
	}

	// Enabled by the environment, endpoint and CA in the panel, CA file
	// gone: the Hub still starts (its settings page repairs the panel).
	g := newFixture(t)
	g.env["MINION_OTEL"] = "on"
	if err := g.st.PutSetting(ctx, "otel_endpoint", "https://panel.example", false, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := g.st.PutSetting(ctx, "otel_ca_file", "/nonexistent/ca.pem", false, "admin"); err != nil {
		t.Fatal(err)
	}
	gs, err := startLogging(t, g)
	if err != nil || gs.Manager.Status()[1].Enabled || gs.Manager.Status()[1].LastError == "" {
		t.Fatalf("start = %v, status %+v", err, gs.Manager.Status())
	}
}

func TestHubInstanceIDIsSafe(t *testing.T) {
	dir := t.TempDir()
	id := hubInstanceID(dir)
	if id == "" || hubInstanceID(dir) != id {
		t.Fatalf("id %q not stable", id)
	}
	bad := t.TempDir()
	junk := strings.Repeat("x", 100)
	if err := os.WriteFile(filepath.Join(bad, "otel-instance-id"), []byte(junk), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := hubInstanceID(bad); got != "" {
		t.Fatalf("junk accepted: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(bad, "otel-instance-id")); string(b) != junk {
		t.Fatal("an invalid file was rewritten")
	}
	if runtime.GOOS == "windows" {
		return
	}
	link := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte(junk), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(link, "otel-instance-id")); err != nil {
		t.Fatal(err)
	}
	if got := hubInstanceID(link); got != "" {
		t.Fatalf("followed a link: %q", got)
	}
	if b, _ := os.ReadFile(target); string(b) != junk {
		t.Fatal("the link target was changed")
	}
}

// The configuration applied after the commit is the one validated before
// it: a certificate file removed in between changes nothing.
func TestApplyUsesTheValidatedConfiguration(t *testing.T) {
	o := newObsFixture(t, nil, nil)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	writeTestCA(t, ca)
	o.settings.OnChange([]string{"syslog"}, func(*config.Resolver) error { return os.Remove(ca) }, nil)
	res := o.set(t, ops.SettingChange{Key: "syslog", Value: "on"}, ops.SettingChange{Key: "syslog_endpoint", Value: "tls://127.0.0.1:1"},
		ops.SettingChange{Key: "syslog_ca_file", Value: ca})
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	if st := o.svc.Manager.Status()[0]; !st.Enabled {
		t.Fatalf("status = %+v", st)
	}
}

func writeTestCA(t *testing.T, path string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A private key pasted into a key path setting is never shown back by
// settings_list.
func TestSettingsListHidesAnInlineKey(t *testing.T) {
	f := newFixture(t)
	f.env["MINION_OTEL_CLIENT_KEY_PATH"] = "-----BEGIN PRIVATE KEY-----\nMIIs3cr3t\n-----END PRIVATE KEY-----"
	svc, err := NewSettingsService(context.Background(), SettingsDeps{Store: f.st, Auditor: f.aud, LookupEnv: f.env.lookup})
	if err != nil {
		t.Fatal(err)
	}
	info := listByKey(t, svc)["otel_client_key_file"]
	if strings.Contains(info.Value, "s3cr3t") || info.Value != config.Redacted || !strings.Contains(info.Error, "not a file path") {
		t.Fatalf("info = %+v", info)
	}
	if !config.SecretPathValue("otel_client_key_file", "a: b") || config.SecretPathValue("otel_client_key_file", "/etc/minion/key.pem") || config.SecretPathValue("otel_ca_file", "-----BEGIN") {
		t.Fatal("SecretPathValue")
	}
}

// stallWriter passes writes through until stalled, then blocks every write.
type stallWriter struct {
	stalled atomic.Bool
	release chan struct{}
}

func (w *stallWriter) Write(p []byte) (int, error) {
	if w.stalled.Load() {
		<-w.release
	}
	return len(p), nil
}

// A local output that stalls during the stop (stderr behind the sinks' loss
// warning) does not keep Run from returning.
func TestRunReturnsWhenTheLocalOutputStallsDuringStop(t *testing.T) {
	w := &stallWriter{release: make(chan struct{})}
	defer close(w.release)
	r := NewRegistry()
	if err := registerCore(r); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("stall", 5, func(_ context.Context, env *Env) error {
		env.OnStop(func(context.Context) error { w.stalled.Store(true); return nil })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: t.TempDir(), TLSOff: true, PublicURL: "http://hub.example.com", Listen: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(w, nil))}
	srv := New(cfg, r)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case <-srv.Started():
	case <-time.After(20 * time.Second):
		t.Fatal("the Hub did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return while the local output stalled")
	}
}

// A traffic setting that is invalid and off already (here from the
// environment, which the panel cannot change) does not keep the operator from
// starting and stopping an unrelated log sink.
func TestInvalidTrafficSettingDoesNotBlockStoppingASink(t *testing.T) {
	o := newObsFixture(t, map[string]string{"MINION_WORKER_TRAFFIC_LOG": "metadata", "MINION_WORKER_TRAFFIC_LOG_TO": "otel"}, nil)
	o.set(t, ops.SettingChange{Key: "syslog", Value: "on"}, ops.SettingChange{Key: "syslog_endpoint", Value: "udp://127.0.0.1:9"})
	if st := o.svc.Manager.Status()[0]; !st.Enabled {
		t.Fatalf("status = %+v", st)
	}
	o.set(t, ops.SettingChange{Key: "syslog", Value: "off"})
	if st := o.svc.Manager.Status()[0]; st.Enabled {
		t.Fatalf("the export was not stopped: %+v", st)
	}
	// A change that makes the traffic setting invalid is still refused.
	o2 := newObsFixture(t, nil, nil)
	if _, err := o2.settings.Set(context.Background(), admin, batchArgs(
		ops.SettingChange{Key: "worker_traffic_log", Value: "metadata"}, ops.SettingChange{Key: "worker_traffic_log_to", Value: "syslog"})); err == nil {
		t.Fatal("a new invalid traffic setting was accepted")
	}
}

// The same holds when the environment holds an unsupported enum value.
func TestInvalidEnvironmentEnumDoesNotBlockStoppingASink(t *testing.T) {
	for _, env := range []map[string]string{{"MINION_WORKER_TRAFFIC_LOG": "payload"}, {"MINION_OTEL_METRICS": "full"}} {
		o := newObsFixture(t, env, nil)
		o.set(t, ops.SettingChange{Key: "syslog", Value: "on"}, ops.SettingChange{Key: "syslog_endpoint", Value: "udp://127.0.0.1:9"})
		o.set(t, ops.SettingChange{Key: "syslog", Value: "off"})
		if st := o.svc.Manager.Status()[0]; st.Enabled {
			t.Fatalf("%v: the export was not stopped: %+v", env, st)
		}
	}
}
