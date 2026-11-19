package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The Hub's traffic log is live: a destination whose sink is off is refused
// before anything is stored; local turns it on at once, narrowed to the
// listed Workers.
func TestHubTrafficLogIsLiveAndValidated(t *testing.T) {
	o := newObsFixture(t, nil, nil)
	_, err := o.settings.Set(context.Background(), admin, batchArgs(
		ops.SettingChange{Key: "worker_traffic_log", Value: "metadata"}, ops.SettingChange{Key: "worker_traffic_log_to", Value: "syslog"}))
	if err == nil || !strings.Contains(err.Error(), "syslog") {
		t.Fatalf("err = %v", err)
	}
	if recs, _ := o.st.ListSettings(context.Background()); len(recs) != 0 {
		t.Fatalf("stored: %+v", recs)
	}
	o.set(t, ops.SettingChange{Key: "worker_traffic_log", Value: "metadata"}, ops.SettingChange{Key: "worker_traffic_log_workers", Value: "pi"})
	tel := hubTelemetry{log: o.svc.Traffic}
	f := transport.TapFrame{Kind: transport.KindOp, Stream: 1, Sent: true, Body: []byte(`{"type":"op.request","op":"sessions_list","request_id":"r"}`), Size: 60}
	tel.FrameTap("w-1", "pi").TapFrame(f)
	tel.FrameTap("w-2", "other").TapFrame(f)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(o.local.String(), `msg="worker traffic"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no traffic record: %s", o.local.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if strings.Count(o.local.String(), `msg="worker traffic"`) != 1 || !strings.Contains(o.local.String(), "worker_id=w-1") {
		t.Fatalf("local = %s", o.local.String())
	}
	o.set(t, ops.SettingChange{Key: "worker_traffic_log", Value: "off"})
	tel.FrameTap("w-1", "pi").TapFrame(f)
	time.Sleep(50 * time.Millisecond)
	if strings.Count(o.local.String(), `msg="worker traffic"`) != 1 {
		t.Fatal("logged after off")
	}
}

// otel_metrics needs the OpenTelemetry sink: a batch that turns metrics on
// without it is refused before anything is stored.
func TestHubMetricsNeedOTel(t *testing.T) {
	o := newObsFixture(t, nil, nil)
	_, err := o.settings.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: "otel_metrics", Value: "basic"}))
	if err == nil || !strings.Contains(err.Error(), "otel_metrics") {
		t.Fatalf("err = %v", err)
	}
	if recs, _ := o.st.ListSettings(context.Background()); len(recs) != 0 {
		t.Fatalf("stored: %+v", recs)
	}
}
