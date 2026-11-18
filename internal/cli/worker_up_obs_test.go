package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
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

// A sink setting that does not validate fails the Worker start, naming the
// key, never the value.
func TestStartWorkerSinksRefusesAnInvalidSetting(t *testing.T) {
	res := &config.Resolver{Role: config.ScopeWorker, File: config.MapSource{"syslog": "on", "syslog_endpoint": "http://u:s3cr3t@h:1"},
		LookupEnv: func(string) (string, bool) { return "", false }}
	_, err := startWorkerSinks(t.Context(), res, nil)
	if err == nil || !strings.Contains(err.Error(), "syslog_endpoint") || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("err = %v", err)
	}
}
