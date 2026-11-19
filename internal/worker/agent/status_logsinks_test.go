package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// worker status shows the remote log sinks of the running Worker, in JSON and
// in text, after the round trip over the local socket.
func TestStatusShowsLogSinks(t *testing.T) {
	ts := ops.Timestamp(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	sinks := []ops.LogSinkStatus{
		{Name: "syslog", Enabled: true, Destination: "tls://logs.example:6514", Level: "warn", Generation: 1, Sent: 7, LastSuccess: &ts,
			Dropped: map[string]uint64{"queue_full": 2}},
		{Name: "otel", Generation: 0},
	}
	src := LiveStatusSource{LogSinks: func() []ops.LogSinkStatus { return sinks }}
	ls, err := src.LiveStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reply := NewLiveStatusReply("/state", ls)
	b, _ := json.Marshal(reply)
	var back LiveStatusReply
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	live, err := back.LiveStatus()
	if err != nil {
		t.Fatal(err)
	}
	r := BuildStatus(context.Background(), StatusInputs{Version: "v", Live: fakeLive{ls: live}})
	var js bytes.Buffer
	if err := r.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(js.Bytes(), &obj)
	list, _ := obj["log_sinks"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["destination"] != "tls://logs.example:6514" {
		t.Fatalf("json = %s", js.String())
	}
	var txt bytes.Buffer
	_ = r.WriteText(&txt)
	for _, want := range []string{"Log export (syslog):", "tls://logs.example:6514 at warn and above; sent 7, queued 0, dropped 2", "Log export (otel):", "off"} {
		if !strings.Contains(txt.String(), want) {
			t.Fatalf("text lacks %q:\n%s", want, txt.String())
		}
	}
}

// The log sinks module stops the sinks last, within its share of the budget.
func TestLogSinksModuleStopsWithinItsShare(t *testing.T) {
	var got time.Duration
	stop, err := startLogSinks(context.Background(), &Env{rt: &Runtime{settings: Settings{StopLogSinks: func(ctx context.Context) error {
		dl, _ := ctx.Deadline()
		got = time.Until(dl)
		return nil
	}}}})
	if err != nil || stop == nil {
		t.Fatalf("start = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = stop(ctx)
	if got > time.Second+50*time.Millisecond || got <= 0 {
		t.Fatalf("stop budget %s, want about 1 s (1.5 s - 0.5 s)", got)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), DefaultStopTimeout)
	defer cancel2()
	_ = stop(ctx2)
	if got > LogSinksStopLimit || got < LogSinksStopLimit-100*time.Millisecond {
		t.Fatalf("stop budget %s, want %s", got, LogSinksStopLimit)
	}
}

func TestLogSinksStopNeverFailsTheStop(t *testing.T) {
	stop, _ := startLogSinks(context.Background(), &Env{rt: &Runtime{settings: Settings{StopLogSinks: func(context.Context) error {
		return context.DeadlineExceeded
	}}}})
	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop = %v", err)
	}
}
