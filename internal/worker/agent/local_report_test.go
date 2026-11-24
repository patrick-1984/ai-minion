package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The executor reports what it really ran: one report for a write and none
// for a duplicate answered from the cache; a refusal before resolution names
// no session (the raw argument may be anything a caller typed).
func TestExecutorReportsLocalWritesFromItsOwnOutcome(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	var mu sync.Mutex
	var got []LocalWrite
	e.ex.OnLocalWrite(func(lw LocalWrite) { mu.Lock(); got = append(got, lw); mu.Unlock() })

	args := sendText("work", secretText, false)
	e.mustWrite(ops.OpSendText, args)
	e.mustWrite(ops.OpSendText, args) // cached duplicate
	if _, err := e.write(ops.OpSendText, sendText("sk-not-a-session", "x", false)); err == nil {
		t.Fatal("a missing target was accepted")
	}
	// A Hub request (with an origin) is not a local write.
	raw := mustJSON(t, sendText("work", "y", false))
	_, _ = e.ex.Execute(context.Background(), Request{Op: ops.OpSendText, Actions: allActions, Args: raw, Origin: &proto.Origin{}})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("reports = %+v, want 2 (the write and the refusal)", got)
	}
	if g := got[0]; g.Op != ops.OpSendText || g.Session != "work" || g.Outcome != "delivered" || g.RequestID != args.RequestID {
		t.Errorf("write report = %+v", g)
	}
	if g := got[1]; g.Outcome != "refused" || g.Reason != "target_not_found" || g.Session != "" {
		t.Errorf("refusal report = %+v", g)
	}
	for _, g := range got {
		if b, _ := json.Marshal(g); strings.Contains(string(b), secretText) || strings.Contains(string(b), "sk-not") {
			t.Errorf("report carries caller text: %s", b)
		}
	}
}

type reportCaller struct {
	mu      sync.Mutex
	got     []proto.LocalAuditArgs
	keyless []bool
	done    chan struct{}
	welcome HubWelcome
	up      bool
	block   chan struct{}
}

func (r *reportCaller) Do(ctx context.Context, op string, args, _ any) error {
	if r.block != nil {
		<-r.block
	}
	if op == proto.OpLocalAuditV1 {
		skip, _ := ctx.Value(noCallKey{}).(bool)
		r.mu.Lock()
		r.got = append(r.got, args.(proto.LocalAuditArgs))
		r.keyless = append(r.keyless, skip)
		r.mu.Unlock()
		r.done <- struct{}{}
	}
	return nil
}

func (r *reportCaller) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func (r *reportCaller) Welcome() (HubWelcome, bool) { return r.welcome, r.up }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReporterSendsKeylessOnlyToAHubThatOffersIt(t *testing.T) {
	rc := &reportCaller{done: make(chan struct{}, 4), up: true, welcome: HubWelcome{LocalAuditV1: true}}
	r := newLocalReporter(func() PipeCaller { return rc }, quietLog())
	id := ops.NewUUIDv7()
	r.Enqueue(LocalWrite{Op: ops.OpSendText, Outcome: "delivered", Session: "mt-claude", RequestID: id})
	select {
	case <-rc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no report")
	}
	rc.mu.Lock()
	g, keyless := rc.got[0], rc.keyless[0]
	rc.mu.Unlock()
	if g.Op != ops.OpSendText || g.Session != "mt-claude" || g.Outcome != "delivered" || g.RequestID != id || g.SourceSession != "" || !keyless {
		t.Errorf("report = %+v keyless=%v", g, keyless)
	}

	// A v0.20.1 Hub does not offer the op: nothing is sent.
	old := &reportCaller{done: make(chan struct{}, 1), up: true}
	r2 := newLocalReporter(func() PipeCaller { return old }, quietLog())
	r2.Enqueue(LocalWrite{Op: ops.OpSendText, Outcome: "delivered"})
	select {
	case <-old.done:
		t.Fatal("reported to a Hub that does not offer local_audit_v1")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReporterQueueIsBoundedAndCountsDrops(t *testing.T) {
	rc := &reportCaller{done: make(chan struct{}, 1000), up: true, welcome: HubWelcome{LocalAuditV1: true}, block: make(chan struct{})}
	r := newLocalReporter(func() PipeCaller { return rc }, quietLog())
	for range localReportQueue + 50 {
		r.Enqueue(LocalWrite{Op: ops.OpSendText, Outcome: "delivered"})
	}
	if d := r.Dropped(); d < 49 {
		t.Errorf("dropped = %d, want at least 49", d)
	}
	close(rc.block)
}

// The report speaks as the Worker of the connection: the call key file is
// not read (an unreadable one does not lose the report) and never sent.
func TestKeylessCallDoesNotReadTheKey(t *testing.T) {
	read := false
	c := NewCallClient(CallConfig{Key: func() (string, error) { read = true; return "", io.ErrUnexpectedEOF }})
	f, err := c.request(withoutCallKey(context.Background()), proto.OpLocalAuditV1, proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered"})
	if err != nil || read || f.APIKey != "" {
		t.Errorf("request = %+v, err = %v, key read = %v", f, err, read)
	}
	if _, err := c.request(context.Background(), proto.OpLocalAuditV1, nil); err == nil {
		t.Error("an ordinary call ignored the unreadable key")
	}
}
