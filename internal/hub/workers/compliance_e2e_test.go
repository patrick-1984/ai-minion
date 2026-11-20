package workers

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/obs/otlptest"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

func otlpCfg(url, level string) config.Sinks {
	return config.Sinks{OTLP: config.OTLPConfig{Enabled: true, LogsURL: url, Level: level, Digest: url + level}}
}

const e2eKey = "mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst" // Minion API key format

// TestComplianceMonitoringEndToEnd drives a fake live session through the four
// modes, a retarget, a level that filters the events and a stalled collector,
// against a real OTLP sink and the real Router.
func TestComplianceMonitoringEndToEnd(t *testing.T) {
	var local bytes.Buffer
	fan := logx.NewFanout()
	fan.Set(logx.SlotLocal, slog.NewTextHandler(&local, &slog.HandlerOptions{Level: slog.LevelDebug}), slog.LevelDebug)
	m := obs.New(fan, obs.Process{Service: "minion-hub"})
	recv1, recv2 := otlptest.New(t), otlptest.New(t)
	ctx := context.Background()
	if err := m.Apply(ctx, otlpCfg(recv1.URL(), "info")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = m.Shutdown(sctx)
	})
	mon := compliance.New(m.SessionSink)
	r := newRig(t, func(c *RouterConfig) { c.Monitor = mon })

	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	snapshot := func() {
		t.Helper()
		r.script(newFakeStream(resultFrame(t, ops.SessionSnapshotResult{
			Pane:   ops.PaneInfo{Session: "minion-1", ID: "%1"},
			Screen: ops.Screen{Width: 80, Height: 2, Lines: []string{"$ deploy", "token " + e2eKey}},
		})))
		if _, err := r.rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSessionSnapshot, WorkerID: target.WorkerID,
			Session: target.SessionName, Args: ops.SessionSnapshotArgs{Target: target}}); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
	}
	send := func(p func() CallRequest) {
		t.Helper()
		r.script(newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
		if _, err := r.rt.Call(ctx, p()); err != nil {
			t.Fatalf("send_text: %v", err)
		}
	}
	sendText := func(text string) {
		send(func() CallRequest {
			return CallRequest{Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
				Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: text, Submit: true}}
		})
	}

	// off (the default): the session works, nothing is recorded.
	sendText("echo off")
	snapshot()

	// actions: metadata of both calls, no content.
	mon.SetMode(compliance.Actions)
	snapshot()
	sendText("echo actions")
	ev := recv1.WaitEvents(t, 2)
	for _, e := range ev {
		if e.Name != compliance.EventAction || e.Severity != "INFO" || e.Body != "" {
			t.Fatalf("actions mode sent %+v", e)
		}
	}
	if a := ev[0].Attrs; a["minion.op"] != "session_snapshot" || a["minion.outcome"] != "ok" || a["minion.session"] != "minion-1" ||
		a["minion.worker.id"] != wAlicePi1.ID || a["minion.user.id"] != idAlice || a["minion.principal.kind"] == "" {
		t.Errorf("action attributes = %v", a)
	}
	if ev[1].Attrs["minion.op"] != "send_text" || ev[1].Attrs["minion.outcome"] != string(ops.OutcomeDelivered) {
		t.Errorf("send_text action = %v", ev[1].Attrs)
	}

	// output: the screen is added (masked), typed input is not.
	mon.SetMode(compliance.Output)
	snapshot()
	sendText("echo output")
	ev = recv1.WaitEvents(t, 2+3)[2:]
	if ev[0].Name != compliance.EventAction || ev[1].Name != compliance.EventOutput || ev[2].Name != compliance.EventAction {
		t.Fatalf("output mode events = %+v", ev)
	}
	if !strings.Contains(ev[1].Body, "$ deploy") || strings.Contains(ev[1].Body, e2eKey) || !strings.Contains(ev[1].Body, "[redacted]") {
		t.Errorf("output body not masked: %q", ev[1].Body)
	}

	// full: the submitted text is added.
	mon.SetMode(compliance.Full)
	sendText("echo " + e2eKey)
	// a call that was denied (alice on bob's Worker) records nothing.
	if _, err := r.rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSendText, WorkerID: wBobPi1.ID, Session: "s",
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: wBobPi1.ID, SessionName: "s"}, Text: "x"}}); err == nil {
		t.Fatal("a call on another user's Worker was allowed")
	}
	ev = recv1.WaitEvents(t, 5+2)[5:]
	if ev[0].Name != compliance.EventAction || ev[1].Name != compliance.EventInput {
		t.Fatalf("full mode events = %+v", ev)
	}
	if ev[1].Attrs["minion.input.kind"] != "text" || !strings.HasPrefix(ev[1].Body, "echo [redacted]") || strings.Contains(ev[1].Body, e2eKey) {
		t.Errorf("input event = %+v", ev[1])
	}

	// retarget: the next events go to the new collector only.
	if err := m.Apply(ctx, otlpCfg(recv2.URL(), "info")); err != nil {
		t.Fatal(err)
	}
	sendText("after retarget")
	recv2.WaitEvents(t, 2)
	if n := len(recv1.Events()); n != 7 {
		t.Errorf("the old collector got %d events, want 7", n)
	}

	// otel_level warn filters INFO events: nothing is sent, the status says so.
	if err := m.Apply(ctx, otlpCfg(recv2.URL(), "warn")); err != nil {
		t.Fatal(err)
	}
	if got := mon.Status(); !strings.Contains(got, "otel_level") {
		t.Errorf("status = %q, want a note about otel_level", got)
	}
	sendText("filtered")
	if err := m.Apply(ctx, otlpCfg(recv2.URL(), "info")); err != nil {
		t.Fatal(err)
	}

	// a stalled collector: the queue fills, new events are dropped and counted,
	// and session operations keep working at once.
	recv2.Stall()
	t.Cleanup(recv2.Release)
	for i := 0; i < 1500; i++ {
		mon.Emit(compliance.Full, compliance.EventInput, "bulk", compliance.KV{Key: "minion.op", Value: "send_text"})
	}
	start := time.Now()
	sendText("during the stall")
	if time.Since(start) > 2*time.Second {
		t.Errorf("a session operation waited %v for a stalled collector", time.Since(start))
	}
	var dropped uint64
	for _, st := range m.Status() {
		if st.Name == obs.SinkOTel {
			dropped = st.Dropped["queue_full"]
		}
	}
	if dropped == 0 {
		t.Error("no drops counted for a stalled collector")
	}
	if strings.Contains(local.String(), e2eKey) || strings.Contains(local.String(), "echo ") || strings.Contains(local.String(), "$ deploy") {
		t.Errorf("session content reached the local log: %s", local.String())
	}
}
