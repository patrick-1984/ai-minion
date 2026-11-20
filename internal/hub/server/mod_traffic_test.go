package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
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

// compliance_monitoring needs the otel sink with an https or loopback
// endpoint; it is live; switching the sink off ends capture without being
// refused; and it is stored like any setting.
func TestHubComplianceMonitoringSetting(t *testing.T) {
	o := newObsFixture(t, nil, nil)
	ctx := context.Background()
	refused := func(want string, changes ...ops.SettingChange) {
		t.Helper()
		_, err := o.settings.Set(ctx, admin, batchArgs(changes...))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
	refused("needs", ops.SettingChange{Key: "compliance_monitoring", Value: "full"})
	refused("https", ops.SettingChange{Key: "compliance_monitoring", Value: "output"},
		ops.SettingChange{Key: "otel", Value: "on"}, ops.SettingChange{Key: "otel_endpoint", Value: "http://collector.example:4318"})
	if recs, _ := o.st.ListSettings(ctx); len(recs) != 0 {
		t.Fatalf("stored: %+v", recs)
	}
	sink := newOTLPSink(t, "")
	o.set(t, ops.SettingChange{Key: "otel", Value: "on"}, ops.SettingChange{Key: "otel_endpoint", Value: sink.srv.URL},
		ops.SettingChange{Key: "compliance_monitoring", Value: "full"})
	if got := o.svc.Compliance.Mode(); got != compliance.Full {
		t.Fatalf("mode = %v", got)
	}
	// A retarget to a clear-text remote endpoint is refused while capture is on.
	refused("https", ops.SettingChange{Key: "otel_endpoint", Value: "http://collector.example:4318"})
	o.set(t, ops.SettingChange{Key: "compliance_monitoring", Value: "actions"})
	if got := o.svc.Compliance.Mode(); got != compliance.Actions {
		t.Fatalf("mode = %v", got)
	}
	// Switching the sink off is never refused; capture ends.
	o.set(t, ops.SettingChange{Key: "otel", Value: "off"})
	if got := o.svc.Compliance.Mode(); got != compliance.Off {
		t.Fatalf("mode = %v after the sink was switched off", got)
	}
}

// The banner and the status follow the live level; the environment's origin is
// shown; a Worker the Hub does not know is not user-managed for content.
func TestComplianceLiveLevelOriginAndContentManaged(t *testing.T) {
	mon := compliance.New(func() compliance.Sink { return nil })
	level := monitoringLevelFunc(mon)
	if level() != "off" {
		t.Fatalf("level = %q", level())
	}
	mon.SetMode(compliance.Full)
	if level() != "full" {
		t.Fatalf("the banner level is frozen: %q", level())
	}

	sink := newOTLPSink(t, "")
	o := newObsFixture(t, map[string]string{"MINION_OTEL": "on", "MINION_OTEL_ENDPOINT": sink.srv.URL, "MINION_COMPLIANCE_MONITORING": "full"}, nil)
	if o.svc.Compliance.Mode() != compliance.Full || !strings.Contains(o.svc.Compliance.Status(), "the environment") {
		t.Fatalf("mode %v, status %q", o.svc.Compliance.Mode(), o.svc.Compliance.Status())
	}
	if !strings.Contains(o.local.String(), "MINION_COMPLIANCE_MONITORING") {
		t.Errorf("no start-up line about the environment setting: %s", o.local.String())
	}
	if ok, err := contentManaged(o.st)(context.Background(), "no-such-worker"); ok || err != nil {
		t.Fatalf("an unknown Worker counts as user-managed: %v %v", ok, err)
	}
}
