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
