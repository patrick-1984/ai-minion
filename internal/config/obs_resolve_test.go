package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func hubRes(env, panel map[string]string) *Resolver {
	return &Resolver{Role: ScopeHub, Panel: MapSource(panel), LookupEnv: func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}}
}

func workerRes(env, file map[string]string) *Resolver {
	return &Resolver{Role: ScopeWorker, File: MapSource(file), LookupEnv: func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}}
}

func TestObsSettingsDeclared(t *testing.T) {
	for _, k := range ObsSettingKeys {
		s, ok := Default.Lookup(k)
		if !ok || s.Help == "" {
			t.Fatalf("%s is not declared with help", k)
		}
	}
	if s, _ := Default.Lookup(KeyOTelHeaders); !s.Secret || s.Scope != ScopeHub || s.FileEnvName() != "MINION_OTEL_HEADERS_FILE" {
		t.Errorf("otel_headers = %+v", s)
	}
	if s, _ := Default.Lookup(KeyOTelHeadersPath); s.Secret || s.Scope != ScopeWorker || s.EnvName() != "MINION_OTEL_HEADERS_PATH" {
		t.Errorf("otel_headers_path = %+v", s)
	}
	if s, _ := Default.Lookup(KeySyslogClientKeyFile); s.EnvName() != "MINION_SYSLOG_CLIENT_KEY_PATH" || s.FlagName() != "syslog-client-key-path" {
		t.Errorf("syslog_client_key_file env %s flag %s", s.EnvName(), s.FlagName())
	}
}

func TestOTLPEndpointPrecedence(t *testing.T) {
	cases := []struct {
		name          string
		env, panel    map[string]string
		logs, metrics string
	}{
		{"minion key wins over every alias",
			map[string]string{EnvOTLPLogsEndpoint: "http://l:1/x", EnvOTLPEndpoint: "http://g:2"},
			map[string]string{"otel_endpoint": "http://m:4318"}, "http://m:4318/v1/logs", "http://m:4318/v1/metrics"},
		{"env key wins over the panel",
			map[string]string{"MINION_OTEL_ENDPOINT": "https://e:4318/base/"},
			map[string]string{"otel_endpoint": "http://m:4318"}, "https://e:4318/base/v1/logs", "https://e:4318/base/v1/metrics"},
		{"per-signal alias used as is",
			map[string]string{EnvOTLPLogsEndpoint: "http://l:1/custom/logs", EnvOTLPEndpoint: "http://g:2/"},
			nil, "http://l:1/custom/logs", "http://g:2/v1/metrics"},
		{"general alias plus path, trailing slash trimmed",
			map[string]string{EnvOTLPEndpoint: "http://g:2/otlp/"}, nil, "http://g:2/otlp/v1/logs", "http://g:2/otlp/v1/metrics"},
		{"metrics alias", map[string]string{EnvOTLPEndpoint: "http://g:2", EnvOTLPMetricsEndpoint: "http://mm:3/m"},
			nil, "http://g:2/v1/logs", "http://mm:3/m"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			panel := map[string]string{"otel": "on"}
			for k, v := range c.panel {
				panel[k] = v
			}
			s, err := ResolveSinks(hubRes(c.env, panel), SinkFiles{})
			if err != nil {
				t.Fatal(err)
			}
			if s.OTLP.LogsURL != c.logs || s.OTLP.MetricsURL != c.metrics {
				t.Fatalf("logs %q metrics %q", s.OTLP.LogsURL, s.OTLP.MetricsURL)
			}
		})
	}
	if _, err := ResolveSinks(hubRes(nil, map[string]string{"otel": "on"}), SinkFiles{}); err == nil || !strings.Contains(err.Error(), "no endpoint") {
		t.Fatalf("missing endpoint: %v", err)
	}
}

func TestOTLPEndpointErrorsNeverEchoTheValue(t *testing.T) {
	for _, bad := range []string{"ftp://h:1", "http://user:s3cr3t@h:1", "http://h:1/?tok=s3cr3t", "http://:1"} {
		_, err := ResolveSinks(hubRes(map[string]string{EnvOTLPEndpoint: bad}, map[string]string{"otel": "on"}), SinkFiles{})
		if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), EnvOTLPEndpoint) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestHeaderEnvIsIgnoredWithAWarning(t *testing.T) {
	env := map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer s3cr3t,bad", EnvOTLPEndpoint: "http://127.0.0.1:4318"}
	s, err := ResolveSinks(hubRes(env, map[string]string{"otel": "on", "otel_headers": "X-Org: one"}), SinkFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.OTLP.Headers) != 1 || s.OTLP.Headers["X-Org"] != "one" {
		t.Fatalf("headers = %v", s.OTLP.Headers)
	}
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "OTEL_EXPORTER_OTLP_HEADERS is ignored") || strings.Contains(s.Warnings[0], "s3cr3t") {
		t.Fatalf("warnings = %q", s.Warnings)
	}
}

func TestParseHeaderLines(t *testing.T) {
	h, err := ParseHeaderLines("# comment\n\nAuthorization: Bearer abc:def\r\nX-Scope-OrgID:  tenant \n")
	if err != nil || h["Authorization"] != "Bearer abc:def" || h["X-Scope-Orgid"] != "tenant" || len(h) != 2 {
		t.Fatalf("h = %v, %v", h, err)
	}
	for in, line := range map[string]string{
		"Good: 1\nno colon s3cr3t":    "line 2",
		"Bad Name: s3cr3t":            "line 1",
		"X: ok\n\nX-A: s3cr3t\x01end": "line 3",
		": s3cr3t":                    "line 1",
	} {
		_, err := ParseHeaderLines(in)
		if err == nil || !strings.Contains(err.Error(), line) || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%q: err = %v", in, err)
		}
	}
}

func TestSyslogValidation(t *testing.T) {
	ok, err := ResolveSinks(hubRes(nil, map[string]string{"syslog": "on", "syslog_endpoint": "tcp://logs.example:6514", "syslog_level": "warn"}), SinkFiles{})
	if err != nil || ok.Syslog.Network != "tcp" || ok.Syslog.Addr != "logs.example:6514" || ok.Syslog.MaxBytes != 2048 || ok.Syslog.Level != "warn" {
		t.Fatalf("syslog = %+v, %v", ok.Syslog, err)
	}
	off, err := ResolveSinks(hubRes(nil, map[string]string{"syslog_endpoint": "garbage"}), SinkFiles{})
	if err != nil || off.Syslog.Enabled {
		t.Fatalf("a sink that is off is not checked: %+v %v", off.Syslog, err)
	}
	for panel, want := range map[string]string{
		"":                              "syslog_endpoint is not set",
		"syslog_endpoint=http://h:1":    "scheme",
		"syslog_endpoint=udp://h":       "no port",
		"syslog_endpoint=udp://h:99999": "port",
		"syslog_max_bytes=100":          "syslog_max_bytes",
		"syslog_ca_file=/x":             "needs a tls:// endpoint",
		"syslog_endpoint=tcp://u:p@h:1": "syslog_endpoint",
		"syslog_endpoint=tls://h:1;syslog_client_cert_file=/c": "set both",
	} {
		p := map[string]string{"syslog": "on", "syslog_endpoint": "udp://h:514"}
		for _, kv := range strings.Split(panel, ";") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				p[k] = v
			} else if kv == "" && panel == "" {
				delete(p, "syslog_endpoint")
			}
		}
		_, err := ResolveSinks(hubRes(nil, p), SinkFiles{})
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "u:p") {
			t.Errorf("%q: err = %v, want %q", panel, err, want)
		}
	}
}

func writePEMPair(t *testing.T, dir string, keyMode os.FileMode) (certPath, keyPath string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "c"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	certPath, keyPath = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), keyMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, keyMode); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestClientKeyModeAndPair(t *testing.T) {
	dir := t.TempDir()
	cert, key := writePEMPair(t, dir, 0o600)
	p := map[string]string{"syslog": "on", "syslog_endpoint": "tls://h:6514", "syslog_ca_file": cert,
		"syslog_client_cert_file": cert, "syslog_client_key_file": key}
	s, err := ResolveSinks(hubRes(nil, p), SinkFiles{})
	if err != nil || s.Syslog.ClientCert == nil || s.Syslog.RootCAs == nil || s.Syslog.TLSConfig().ServerName != "h" {
		t.Fatalf("mTLS config: %+v %v", s.Syslog, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSinks(hubRes(nil, p), SinkFiles{}); err == nil || !strings.Contains(err.Error(), "mode 0600") ||
		!strings.Contains(err.Error(), "syslog_client_key_file") || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("a 0644 key file was accepted: %v", err)
	}
}

func TestWorkerHeadersFromPath(t *testing.T) {
	dir := t.TempDir()
	hp := filepath.Join(dir, "h")
	if err := os.WriteFile(hp, []byte("Authorization: Bearer t0k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := map[string]string{"otel": "on", "otel_endpoint": "https://c.example", "otel_headers_path": hp}
	s, err := ResolveSinks(workerRes(nil, file), SinkFiles{})
	if err != nil || s.OTLP.Headers["Authorization"] != "Bearer t0k" {
		t.Fatalf("headers %v, %v", s.OTLP.Headers, err)
	}
	if err := os.WriteFile(hp, []byte("bad line t0k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSinks(workerRes(nil, file), SinkFiles{}); err == nil || !strings.Contains(err.Error(), "line 1") || strings.Contains(err.Error(), "t0k") {
		t.Fatalf("err = %v", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.WriteFile(hp, []byte("A: b\n"), 0o600)
		_ = os.Chmod(hp, 0o640)
		if _, err := ResolveSinks(workerRes(nil, file), SinkFiles{}); err == nil || !strings.Contains(err.Error(), "otel_headers_path") {
			t.Fatalf("a group-readable header file was accepted: %v", err)
		}
	}
}

func TestSinkDigestChangesWithHeaders(t *testing.T) {
	p := map[string]string{"otel": "on", "otel_endpoint": "http://127.0.0.1:4318", "otel_headers": "A: 1"}
	a, _ := ResolveSinks(hubRes(nil, p), SinkFiles{})
	p["otel_headers"] = "A: 2"
	b, _ := ResolveSinks(hubRes(nil, p), SinkFiles{})
	if a.OTLP.Digest == "" || a.OTLP.Digest == b.OTLP.Digest {
		t.Fatal("the digest ignores the headers")
	}
	if strings.Contains(a.OTLP.Digest, "A: 1") {
		t.Fatal("the digest holds text")
	}
}

func TestHTTPToRemoteHostWarns(t *testing.T) {
	s, err := ResolveSinks(hubRes(nil, map[string]string{"otel": "on", "otel_endpoint": "http://collector.example:4318"}), SinkFiles{})
	if err != nil || len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "unencrypted") {
		t.Fatalf("warnings %q, %v", s.Warnings, err)
	}
}
