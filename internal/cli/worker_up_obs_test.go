package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

// worker up persists every log export setting it was given, the header file
// as a path (the secret stays in the file), so that the service run, which
// reads only the config file, builds the same sinks.
func TestWorkerUpPersistsTheLogSinkSettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the user service fixture is Unix")
	}
	f := newUpFixture(t, false)
	headers := filepath.Join(t.TempDir(), "otel-headers")
	if err := os.WriteFile(headers, []byte("Authorization: Bearer t0ken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env["MINION_OTEL_LEVEL"] = "error"
	_, err := f.run("--syslog", "on", "--syslog-endpoint", "tcp://logs.example:6514", "--syslog-level", "warn",
		"--otel", "on", "--otel-endpoint", "https://collector.example:4318", "--otel-headers-path", headers)
	if err != nil {
		t.Fatalf("%v\n%s", err, f.stderr)
	}
	cfgPath := filepath.Join(f.stateDir(), "config.json")
	got := readWorkerUpConfig(t, cfgPath)
	for k, want := range map[string]string{
		"syslog": "on", "syslog_endpoint": "tcp://logs.example:6514", "syslog_level": "warn",
		"otel": "on", "otel_endpoint": "https://collector.example:4318", "otel_headers_path": headers, "otel_level": "error",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q (config %v)", k, got[k], want, got)
		}
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "t0ken") {
		t.Fatal("the header secret was written to the config file")
	}
	unit, err := os.ReadFile(filepath.Join(f.home, ".config", "systemd", "user", "minion-worker.service"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "OTEL") || strings.Contains(string(unit), "SYSLOG") {
		t.Fatalf("the unit carries sink settings instead of the config file:\n%s", unit)
	}
	// The service run: config file only, no flags, no environment.
	file, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	res := &config.Resolver{Role: config.ScopeWorker, File: file, LookupEnv: func(string) (string, bool) { return "", false }}
	sinks, err := config.ResolveSinks(res, config.SinkFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if !sinks.Syslog.Enabled || sinks.Syslog.Addr != "logs.example:6514" || sinks.Syslog.Level != "warn" ||
		sinks.OTLP.LogsURL != "https://collector.example:4318/v1/logs" || sinks.OTLP.Headers["Authorization"] != "Bearer t0ken" || sinks.OTLP.Level != "error" {
		t.Fatalf("service sinks = %+v / %+v", sinks.Syslog, sinks.OTLP)
	}
}

// A sink setting that does not validate never stops the Worker (an update
// must not look failed for it): both sinks stay off, and the status and
// the local log name the key, never the value.
func TestInvalidSinkSettingLeavesTheWorkerRunning(t *testing.T) {
	res := &config.Resolver{Role: config.ScopeWorker, File: config.MapSource{"syslog": "on", "syslog_endpoint": "http://u:s3cr3t@h:1"},
		LookupEnv: func(string) (string, bool) { return "", false }}
	var local bytes.Buffer
	logger, live, err := logx.NewLiveLocal(logx.Options{Output: &local, Format: logx.FormatText})
	if err != nil {
		t.Fatal(err)
	}
	m, start := newWorkerSinks(res, live, logger, t.TempDir())
	defer func() { _ = m.Shutdown(context.Background()) }()
	start(context.Background())
	st := m.Status()
	if st[0].Enabled || !strings.Contains(st[0].LastError, "syslog_endpoint") || strings.Contains(st[0].LastError, "s3cr3t") {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(local.String(), "log export is off") || strings.Contains(local.String(), "s3cr3t") {
		t.Fatalf("local = %s", local.String())
	}
}

// An enrolled Worker exports with its Worker id as service.instance.id and
// minion.worker.id; an unenrolled one gets no id from the state.
func TestWorkerOTelIDFromTheEnrollment(t *testing.T) {
	if got := workerOTelID(t.Context(), pairedStateDir(t)); got != "w1" {
		t.Fatalf("id = %q", got)
	}
	if got := workerOTelID(t.Context(), t.TempDir()); got != "" {
		t.Fatalf("unenrolled id = %q", got)
	}
}

// worker up checks the sinks as the service will resolve them before it
// saves or restarts anything; a shell-only OTLP variable does not count.
func TestWorkerUpRefusesSinksTheServiceCannotStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the user service fixture is Unix")
	}
	f := newUpFixture(t, false)
	f.env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://127.0.0.1:4318"
	_, err := f.run("--name", "laptop", "--otel", "on")
	if err == nil || !strings.Contains(err.Error(), "--otel-endpoint") || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir(), "config.json")); err == nil {
		if got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json")); got["otel"] != "" {
			t.Fatalf("saved: %v", got)
		}
	}
	for _, c := range f.runCalls() {
		if strings.Contains(c, "restart") {
			t.Fatalf("the service was restarted: %v", f.runCalls())
		}
	}
}

// Relative sink file paths are saved absolute: the service starts in
// another directory.
func TestWorkerUpSavesSinkPathsAbsolute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the user service fixture is Unix")
	}
	f := newUpFixture(t, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hdr"), []byte("A: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if _, err := f.run("--otel", "on", "--otel-endpoint", "https://c.example", "--otel-headers-path", "hdr"); err != nil {
		t.Fatalf("%v\n%s", err, f.stderr)
	}
	got := readWorkerUpConfig(t, filepath.Join(f.stateDir(), "config.json"))
	if got["otel_headers_path"] != filepath.Join(dir, "hdr") {
		t.Fatalf("otel_headers_path = %q", got["otel_headers_path"])
	}
}
