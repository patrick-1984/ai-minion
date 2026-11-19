package api

import (
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
)

// An open page learns about monitoring without a reload: the policy in force
// follows the snapshot, and every later change is an event (A49 T16a).
func TestEventsDeliverMonitoringChanged(t *testing.T) {
	mon := compliance.NewMonitor(compliance.Deps{})
	r := newEvRig(t, func(c *EventsConfig) { c.Monitor = mon })
	c := r.open("alice", "")
	c.expect("snapshot")
	var first MonitoringEvent
	if err := json.Unmarshal([]byte(c.expect("monitoring_changed").data), &first); err != nil {
		t.Fatal(err)
	}
	if first.Version != 0 || first.Level != "off" || first.Notice != "" {
		t.Fatalf("first = %+v", first)
	}

	mon.SetPolicy(compliance.Policy{Version: 4, Level: "output", Input: true, Destination: "https://secret-collector.example:443"})
	m := c.expect("monitoring_changed")
	var got MonitoringEvent
	if err := json.Unmarshal([]byte(m.data), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 4 || got.Level != "output" || got.Input != "on" || got.Traffic != "metadata" ||
		got.Notice != "Sessions are monitored: output, input on (policy v4)" {
		t.Fatalf("event = %+v", got)
	}
	if contains(m.data, "secret-collector") {
		t.Fatalf("the event names the destination: %s", m.data)
	}

	// A burst keeps the latest policy.
	mon.SetPolicy(compliance.Policy{Version: 5, Level: "actions"})
	mon.SetPolicy(compliance.Policy{Version: 6, Level: "off"})
	var last MonitoringEvent
	for i := 0; i < 2; i++ {
		_ = json.Unmarshal([]byte(c.expect("monitoring_changed").data), &last)
		if last.Version == 6 {
			break
		}
	}
	if last.Version != 6 || last.Notice != "" {
		t.Fatalf("last = %+v", last)
	}
}

// Without a Monitor the stream is exactly what it was.
func TestEventsWithoutMonitorHaveNoMonitoringEvent(t *testing.T) {
	r := newEvRig(t, nil)
	c := r.open("alice", "")
	c.expect("snapshot")
	c.none()
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
