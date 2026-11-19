package config

import (
	"slices"
	"strings"
	"testing"
)

func TestResolveTraffic(t *testing.T) {
	sinks := Sinks{Syslog: SyslogConfig{Enabled: true}}
	c, err := ResolveTraffic(hubRes(nil, map[string]string{"worker_traffic_log": "metadata", "worker_traffic_log_to": "local, syslog,local",
		"worker_traffic_log_workers": "w-1, pi"}), sinks)
	if err != nil || !c.Enabled || len(c.To) != 2 || len(c.Workers) != 2 {
		t.Fatalf("c = %+v, %v", c, err)
	}
	if c, err := ResolveTraffic(hubRes(nil, nil), sinks); err != nil || c.Enabled {
		t.Fatalf("default = %+v, %v", c, err)
	}
	for to, want := range map[string]string{"otel": `"otel" is off`, "file": "expected", " , ": "no destination"} {
		_, err := ResolveTraffic(hubRes(nil, map[string]string{"worker_traffic_log": "metadata", "worker_traffic_log_to": to}), sinks)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", to, err)
		}
	}
	// The payload level is not available yet.
	if _, err := ResolveTraffic(hubRes(nil, map[string]string{"worker_traffic_log": "payload"}), sinks); err == nil {
		t.Fatal("payload accepted")
	}
	// A Worker has no Worker filter.
	c, err = ResolveTraffic(workerRes(nil, map[string]string{"worker_traffic_log": "metadata"}), Sinks{})
	if err != nil || len(c.To) != 1 || c.To[0] != "local" {
		t.Fatalf("worker = %+v, %v", c, err)
	}
}

func TestResolveMetricsLevel(t *testing.T) {
	on := Sinks{OTLP: OTLPConfig{Enabled: true, MetricsURL: "http://c:4318/v1/metrics"}}
	if l, err := ResolveMetricsLevel(hubRes(nil, map[string]string{"otel_metrics": "detailed"}), on); err != nil || l != "detailed" {
		t.Fatalf("%q %v", l, err)
	}
	if _, err := ResolveMetricsLevel(hubRes(nil, map[string]string{"otel_metrics": "basic"}), Sinks{}); err == nil || !strings.Contains(err.Error(), `"otel"`) {
		t.Fatalf("err = %v", err)
	}
	if l, err := ResolveMetricsLevel(hubRes(nil, nil), Sinks{}); err != nil || l != "off" {
		t.Fatalf("default %q %v", l, err)
	}
}

// A metrics alias on another host gets its own TLS server name, and a
// cleartext metrics alias to a remote host is warned about.
func TestMetricsEndpointIdentity(t *testing.T) {
	env := map[string]string{EnvOTLPLogsEndpoint: "https://logs.example/v1/logs", EnvOTLPMetricsEndpoint: "https://metrics.example/v1/metrics"}
	s, err := ResolveSinks(hubRes(env, map[string]string{"otel": "on"}), SinkFiles{})
	if err != nil || s.OTLP.MetricsTLSConfig().ServerName != "metrics.example" || s.OTLP.TLSConfig().ServerName != "logs.example" {
		t.Fatalf("%v %v", s.OTLP, err)
	}
	env[EnvOTLPMetricsEndpoint] = "http://metrics.example:4318/v1/metrics"
	s, err = ResolveSinks(hubRes(env, map[string]string{"otel": "on"}), SinkFiles{})
	if err != nil || len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "metrics endpoint") {
		t.Fatalf("warnings %q, %v", s.Warnings, err)
	}
}

// A Worker name with a comma is written in quotes; a value that names no
// Worker is refused instead of selecting every Worker.
func TestTrafficWorkersList(t *testing.T) {
	good := map[string][]string{
		"": nil, "  ": nil, "a, b ,a": {"a", "b"}, `"pi,usb", other`: {"pi,usb", "other"}, `"a ""q"" b"`: {`a "q" b`},
	}
	for in, want := range good {
		got, err := ParseTrafficWorkers(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{",", " , ,", `"unterminated`, "a\nb"} {
		if got, err := ParseTrafficWorkers(in); err == nil {
			t.Errorf("%q accepted: %q", in, got)
		}
	}
	env := map[string]string{EnvOTLPLogsEndpoint: "http://127.0.0.1:4318/v1/logs"}
	s, err := ResolveSinks(hubRes(env, map[string]string{"otel": "on"}), SinkFiles{})
	if err != nil {
		t.Fatal(err)
	}
	r := hubRes(env, map[string]string{"worker_traffic_log": "metadata", "worker_traffic_log_workers": ","})
	if _, err := ResolveTraffic(r, s); err == nil {
		t.Fatal("a separator-only filter resolved")
	}
}
