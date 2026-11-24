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

type reportCaller struct {
	mu   sync.Mutex
	got  []proto.LocalAuditArgs
	done chan struct{}
}

func (r *reportCaller) Do(_ context.Context, op string, args, _ any) error {
	if op == proto.OpLocalAuditV1 {
		r.mu.Lock()
		r.got = append(r.got, args.(proto.LocalAuditArgs))
		r.mu.Unlock()
		r.done <- struct{}{}
	}
	return nil
}

func (r *reportCaller) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func newReportExec(rc PipeCaller) localExecutor {
	return localExecutor{ex: &Executor{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), calls: func() PipeCaller { return rc }}
}

func TestReportLocalWriteSendsOpSessionAndOutcome(t *testing.T) {
	rc := &reportCaller{done: make(chan struct{}, 1)}
	l := newReportExec(rc)
	id := ops.NewUUIDv7()
	args := json.RawMessage(`{"request_id":"` + id + `","target":{"worker_id":"w","session_name":"mt-claude"},"text":"SECRET"}`)
	l.reportLocalWrite(context.Background(), ops.OpSendText, args, json.RawMessage(`{"outcome":"delivered","reason":""}`), nil, "")
	select {
	case <-rc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no report")
	}
	g := rc.got[0]
	if g.Op != ops.OpSendText || g.Session != "mt-claude" || g.Outcome != "delivered" || g.RequestID != id {
		t.Errorf("report = %+v", g)
	}
	if b, _ := json.Marshal(g); strings.Contains(string(b), "SECRET") {
		t.Error("report carries message text")
	}
}

func TestReportLocalWriteSkipsReadsAndReportsRefusals(t *testing.T) {
	rc := &reportCaller{done: make(chan struct{}, 2)}
	l := newReportExec(rc)
	l.reportLocalWrite(context.Background(), ops.OpSessionsList, nil, json.RawMessage(`{}`), nil, "")
	select {
	case <-rc.done:
		t.Fatal("a read was reported")
	case <-time.After(100 * time.Millisecond):
	}
	l.reportLocalWrite(context.Background(), ops.OpSendKeys, json.RawMessage(`{}`), nil, apiErr(proto.CodeForbidden, "", "no"), "")
	select {
	case <-rc.done:
	case <-time.After(2 * time.Second):
		t.Fatal("no report of the refusal")
	}
	if g := rc.got[0]; g.Outcome != "refused" || g.Reason != string(proto.CodeForbidden) {
		t.Errorf("report = %+v", g)
	}
}
