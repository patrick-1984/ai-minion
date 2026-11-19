package config

import (
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
