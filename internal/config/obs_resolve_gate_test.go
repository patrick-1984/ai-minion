package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOTLPURLDelimitersAndCase(t *testing.T) {
	for _, bad := range []string{"http://h:1/base?", "http://h:1/base#", "http://h:1/a%2Fb", "http://h:1/a%20b"} {
		_, err := ResolveSinks(hubRes(nil, map[string]string{"otel": "on", "otel_endpoint": bad}), SinkFiles{})
		if err == nil {
			t.Errorf("%q accepted", bad)
		}
		_, err = ResolveSinks(hubRes(map[string]string{EnvOTLPLogsEndpoint: bad}, map[string]string{"otel": "on"}), SinkFiles{})
		if err == nil {
			t.Errorf("%q accepted as the per-signal endpoint", bad)
		}
	}
	dir := t.TempDir()
	cert, _ := writePEMPair(t, dir, 0o600)
	s, err := ResolveSinks(hubRes(nil, map[string]string{"otel": "on", "otel_endpoint": "HTTPS://Collector.example:4318/Base", "otel_ca_file": cert}), SinkFiles{})
	if err != nil || s.OTLP.LogsURL != "https://Collector.example:4318/Base/v1/logs" || s.OTLP.RootCAs == nil {
		t.Fatalf("mixed-case https: %q %v", s.OTLP.LogsURL, err)
	}
}

func TestHeaderNamesAreCaseInsensitive(t *testing.T) {
	_, err := ParseHeaderLines("Authorization: Bearer a\nauthorization: Bearer s3cr3t")
	if err == nil || !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("err = %v", err)
	}
}

// A mistake that puts the header text into otel_headers_path never shows up
// in the error.
func TestHeaderPathErrorDoesNotQuoteThePath(t *testing.T) {
	file := map[string]string{"otel": "on", "otel_endpoint": "https://c.example", "otel_headers_path": "Authorization: Bearer s3cr3t"}
	_, err := ResolveSinks(workerRes(nil, file), SinkFiles{})
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "otel_headers_path") {
		t.Fatalf("err = %v", err)
	}
}

// Credentials from the environment stay with an endpoint from the
// environment: a panel endpoint cannot redirect them.
func TestPanelEndpointCannotTakeEnvironmentCredentials(t *testing.T) {
	dir := t.TempDir()
	hf := filepath.Join(dir, "h")
	if err := os.WriteFile(hf, []byte("Authorization: Bearer t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(p string) ([]byte, error) { return os.ReadFile(p) }
	env := map[string]string{"MINION_OTEL_HEADERS_FILE": hf}
	res := hubRes(env, map[string]string{"otel": "on", "otel_endpoint": "https://other.example"})
	res.ReadFile = read
	if _, err := ResolveSinks(res, SinkFiles{}); err == nil || !strings.Contains(err.Error(), "MINION_OTEL_ENDPOINT") {
		t.Fatalf("err = %v", err)
	}
	env["MINION_OTEL_ENDPOINT"] = "https://deployer.example"
	res = hubRes(env, map[string]string{"otel": "on", "otel_endpoint": "https://other.example"})
	res.ReadFile = read
	if s, err := ResolveSinks(res, SinkFiles{}); err != nil || !strings.HasPrefix(s.OTLP.LogsURL, "https://deployer.example") {
		t.Fatalf("env endpoint: %v %v", s.OTLP.LogsURL, err)
	}
}

// A scoped IPv6 endpoint keeps its zone escape, so the exporter can parse it.
func TestIPv6ZoneIsKept(t *testing.T) {
	s, err := ResolveSinks(hubRes(nil, map[string]string{"otel": "on", "otel_endpoint": "http://[fe80::1%25eth0]:4318"}), SinkFiles{})
	if err != nil || s.OTLP.LogsURL != "http://[fe80::1%25eth0]:4318/v1/logs" {
		t.Fatalf("url %q, %v", s.OTLP.LogsURL, err)
	}
}

// Secret and key file errors never quote the path (it can hold the secret
// by mistake).
func TestSecretFileErrorsDoNotQuoteThePath(t *testing.T) {
	inline := "Authorization: Bearer s3cr3t"
	res := hubRes(map[string]string{"MINION_OTEL_HEADERS_FILE": inline}, map[string]string{"otel": "on", "otel_endpoint": "http://127.0.0.1:1"})
	if _, err := ResolveSinks(res, SinkFiles{}); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("headers: %v", err)
	}
	if _, err := res.Resolve(KeyOTelHeaders); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("resolve: %v", err)
	}
	dir := t.TempDir()
	cert, _ := writePEMPair(t, dir, 0o600)
	p := map[string]string{"syslog": "on", "syslog_endpoint": "tls://h:6514", "syslog_client_cert_file": cert,
		"syslog_client_key_file": "-----BEGIN PRIVATE KEY----- s3cr3t"}
	if _, err := ResolveSinks(hubRes(nil, p), SinkFiles{}); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("key: %v", err)
	}
}

// Empty and oversized secret files are reported without their path.
func TestEmptyAndOversizedSecretFilesHideThePath(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "big": strings.Repeat("x", 70<<10)} {
		inline := "/tmp/Authorization: Bearer s3cr3t-" + name
		res := hubRes(map[string]string{"MINION_OTEL_HEADERS_FILE": inline}, nil)
		res.ReadFile = func(string) ([]byte, error) { return []byte(content), nil }
		if _, err := res.Resolve(KeyOTelHeaders); err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
