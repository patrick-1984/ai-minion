package traffic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The payload level adds the redacted frame body to the metadata record: valid
// JSON, at most 12 KiB, no Minion token format, no secret field; the metadata
// level never carries it.
func TestPayloadLevelCarriesTheRedactedBody(t *testing.T) {
	key := "mn_" + strings.Repeat("p", 52)
	var out syncBuf
	_, live, _ := logx.NewLiveLocal(logx.Options{Level: "error", Format: logx.FormatJSON, Output: &out})
	l := New(SideHub, live.Fanout)
	tap := l.Tap(Peer{WorkerID: "w-1", Name: "pi"})
	body := `{"type":"op.request","op":"send_text","request_id":"r-1","args":{"text":"hello ` + key + ` MN-ABCD-EFGH","password":"hunter2"}}`
	frame := transport.TapFrame{Kind: transport.KindOp, Stream: 1, Sent: true, Body: []byte(body), Size: len(body) + 4}

	l.Configure(config.TrafficConfig{Enabled: true, To: []string{config.TrafficToLocal}})
	tap.TapFrame(frame)
	eventually(t, func() bool { return len(records(t, &out)) == 1 })
	if _, ok := records(t, &out)[0]["payload"]; ok {
		t.Fatal("metadata level carried a payload")
	}

	l.Configure(config.TrafficConfig{Enabled: true, Payload: true, To: []string{config.TrafficToLocal}})
	tap.TapFrame(frame)
	eventually(t, func() bool { return len(records(t, &out)) == 2 })
	rec := records(t, &out)[1]
	p, _ := rec["payload"].(string)
	if !json.Valid([]byte(p)) || len(p) > logx.RemotePayloadMax {
		t.Fatalf("payload = %q", p)
	}
	for _, leak := range []string{strings.Repeat("p", 52), "ABCD-EFGH", "hunter2"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("%q reached the output", leak)
		}
	}
	if !strings.Contains(p, "mn_[redacted]") || !strings.Contains(p, `"op":"send_text"`) || rec["op"] != "send_text" {
		t.Fatalf("payload = %s", p)
	}
}

// A large body is cut into a marker that is still JSON; the tap never keeps
// the frame's own bytes (it works on a copy made during the call).
func TestPayloadIsBoundedAndTheTapKeepsNoFrameBytes(t *testing.T) {
	var out syncBuf
	_, live, _ := logx.NewLiveLocal(logx.Options{Level: "error", Format: logx.FormatJSON, Output: &out})
	l := New(SideHub, live.Fanout)
	l.Configure(config.TrafficConfig{Enabled: true, Payload: true, To: []string{config.TrafficToLocal}})
	big, _ := json.Marshal(map[string]any{"type": "op.result", "result": map[string]any{"screen": strings.Repeat("x\n", 40000)}})
	buf := append([]byte(nil), big...)
	l.Tap(Peer{WorkerID: "w"}).TapFrame(transport.TapFrame{Kind: transport.KindOp, Stream: 2, Body: buf, Size: len(buf) + 4})
	for i := range buf { // the transport reuses its buffer after the call
		buf[i] = 'Z'
	}
	eventually(t, func() bool { return len(records(t, &out)) == 1 })
	p, _ := records(t, &out)[0]["payload"].(string)
	if !json.Valid([]byte(p)) || len(p) > logx.RemotePayloadMax || !strings.Contains(p, `"truncated"`) || strings.Contains(p, "Z") {
		t.Fatalf("payload (%d bytes) = %.100s", len(p), p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l.Close(ctx)
}

// Until the gate says yes (the Hub publishes the monitoring policy that
// announces payload capture), no body is recorded even though the filter asks
// for payload: capture starts strictly after the notice.
func TestPayloadWaitsForTheGate(t *testing.T) {
	var out syncBuf
	_, live, _ := logx.NewLiveLocal(logx.Options{Level: "error", Format: logx.FormatJSON, Output: &out})
	l := New(SideHub, live.Fanout)
	open := false
	l.SetPayloadGate(func() bool { return open })
	l.Configure(config.TrafficConfig{Enabled: true, Payload: true, To: []string{config.TrafficToLocal}})
	body := `{"type":"op.request","op":"send_text","args":{"text":"hi"}}`
	tap := l.Tap(Peer{WorkerID: "w"})
	frame := transport.TapFrame{Kind: transport.KindOp, Stream: 1, Sent: true, Body: []byte(body), Size: len(body) + 4}
	tap.TapFrame(frame)
	open = true
	tap.TapFrame(frame)
	eventually(t, func() bool { return len(records(t, &out)) == 2 })
	rs := records(t, &out)
	if _, ok := rs[0]["payload"]; ok {
		t.Fatal("a body was recorded before the gate opened")
	}
	if _, ok := rs[1]["payload"]; !ok {
		t.Fatal("no body after the gate opened")
	}
}
