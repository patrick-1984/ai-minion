package workers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestLocalAuditEvent(t *testing.T) {
	w := ops.NewUUIDv7()
	ev, err := localAuditEvent(w, proto.LocalAuditArgs{Op: ops.OpSendText, Session: "mt-claude", Outcome: "delivered", SourceSession: "wb-1", ReportID: ops.NewUUIDv7()})
	if err != nil {
		t.Fatal(err)
	}
	// The calling session a Worker names is not verified: the actor is the
	// authenticated Worker's local entry point.
	if ev.Actor != "worker:"+w+"/local" || ev.Action != "local.send_text" || ev.Target != w+":mt-claude" || ev.Outcome != "delivered" {
		t.Errorf("event = %+v", ev)
	}
	ev, _ = localAuditEvent(w, proto.LocalAuditArgs{Op: ops.OpSendKeys, Outcome: "refused", Reason: "forbidden", ReportID: ops.NewUUIDv7()})
	if ev.Actor != "worker:"+w+"/local" || ev.Outcome != "refused:forbidden" || ev.Target != w {
		t.Errorf("event = %+v", ev)
	}
	for _, bad := range []string{"sessions_list", "settings_set", "worker_remove", "nope"} {
		if _, err := localAuditEvent(w, proto.LocalAuditArgs{Op: bad, Outcome: "delivered", ReportID: ops.NewUUIDv7()}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for name, a := range map[string]proto.LocalAuditArgs{
		"overlong session": {Op: ops.OpSendText, Outcome: "delivered", Session: strings.Repeat("x", 600)},
		"free outcome":     {Op: ops.OpSendText, Outcome: "sk-live-abcdef"},
		"empty outcome":    {Op: ops.OpSendText},
		"free reason":      {Op: ops.OpSendText, Outcome: "refused", Reason: "password=hunter2"},
		"bad request id":   {Op: ops.OpSendText, Outcome: "delivered", RequestID: "abc"},
		"bad session":      {Op: ops.OpSendText, Outcome: "delivered", Session: "a b\tc"},
	} {
		if _, err := localAuditEvent(w, a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLocalAuditBudgetDropsAndDeduplicates(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLocalAudit(nil)
	l.log = quietLogger()
	l.now = func() time.Time { return now }
	w, other := ops.NewUUIDv7(), ops.NewUUIDv7()
	id := ops.NewUUIDv7()
	if l.admit(w, id) != admitOK || l.admit(w, id) != admitInProgress {
		t.Fatal("a report being recorded was admitted twice")
	}
	l.settle(w, id, true)
	if l.admit(w, id) != admitDuplicate {
		t.Fatal("a repeated delivery of a recorded report was not deduplicated")
	}
	n := 1
	for ; n < 1000; n++ {
		r := ops.NewUUIDv7()
		res := l.admit(w, r)
		if res == admitDropped {
			break
		}
		l.settle(w, r, true)
	}
	if n != localAuditBurst {
		t.Errorf("admitted %d reports before the first drop, want %d", n, localAuditBurst)
	}
	if l.Dropped(w) != 1 {
		t.Errorf("dropped = %d", l.Dropped(w))
	}
	// Another Worker has its own budget; time refills it.
	if l.admit(other, ops.NewUUIDv7()) != admitOK {
		t.Error("another Worker was throttled")
	}
	now = now.Add(5 * time.Second)
	if l.admit(w, ops.NewUUIDv7()) != admitOK {
		t.Error("the budget did not refill")
	}
	// A Worker that reported nothing for an hour is forgotten; one with a
	// report still being recorded is kept.
	now = now.Add(localAuditIdle + time.Minute)
	l.admit(ops.NewUUIDv7(), ops.NewUUIDv7())
	l.mu.Lock()
	_, keptW := l.workers[w]
	_, keptOther := l.workers[other]
	l.mu.Unlock()
	if !keptW || !keptOther {
		t.Error("a Worker with a report in progress was forgotten")
	}
	l.mu.Lock()
	for _, b := range l.workers {
		for k := range b.seen {
			b.seen[k] = true
		}
	}
	l.lastSweep = time.Time{}
	l.mu.Unlock()
	l.admit(ops.NewUUIDv7(), ops.NewUUIDv7())
	l.mu.Lock()
	_, keptW = l.workers[w]
	_, keptOther = l.workers[other]
	left := len(l.workers)
	l.mu.Unlock()
	if keptW || keptOther || left != 2 {
		t.Errorf("after an idle hour: w kept %v, other kept %v, %d Workers (want only the 2 that just reported)", keptW, keptOther, left)
	}
}

// failingRecorder fails the first n records, then records into rec.
type failingRecorder struct {
	mu    sync.Mutex
	fail  int
	evs   []audit.Event
	gate  chan struct{}
	inRec chan struct{}
	rec   func(ctx context.Context, ev audit.Event) error
}

func (f *failingRecorder) Record(ctx context.Context, ev audit.Event) error {
	if f.inRec != nil {
		f.inRec <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("disk full")
	}
	f.evs = append(f.evs, ev)
	if f.rec != nil {
		return f.rec(ctx, ev)
	}
	return nil
}

func reportJSON(t *testing.T, a proto.LocalAuditArgs) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A report whose record failed is not acknowledged by a repeat: the repeat
// records it. A repeat while the first delivery is still being recorded is
// told so, and a later write that reuses the request id is a new report.
func TestLocalAuditAcknowledgesOnlyCommittedReports(t *testing.T) {
	w := ops.NewUUIDv7()
	f := &failingRecorder{fail: 1}
	l := NewLocalAudit(nil)
	l.auditor = f
	l.log = quietLogger()
	req := ops.NewUUIDv7()
	a := proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered", RequestID: req, ReportID: ops.NewUUIDv7()}
	if perr := l.handle(t.Context(), w, reportJSON(t, a)); perr == nil || perr.Code != proto.CodeInternal {
		t.Fatalf("failed record answered %v", perr)
	}
	if perr := l.handle(t.Context(), w, reportJSON(t, a)); perr != nil {
		t.Fatalf("retry: %v", perr)
	}
	if perr := l.handle(t.Context(), w, reportJSON(t, a)); perr != nil {
		t.Fatalf("duplicate: %v", perr)
	}
	// The same request id in a new report (a later execution) is recorded.
	b := a
	b.ReportID = ops.NewUUIDv7()
	if perr := l.handle(t.Context(), w, reportJSON(t, b)); perr != nil {
		t.Fatal(perr)
	}
	if len(f.evs) != 2 {
		t.Fatalf("recorded %d events, want 2 (the retried report and the new one)", len(f.evs))
	}

	// A concurrent repeat while the first delivery is being recorded.
	f.gate, f.inRec = make(chan struct{}), make(chan struct{}, 1)
	c := proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered", ReportID: ops.NewUUIDv7()}
	first := make(chan *proto.Error, 1)
	go func() { first <- l.handle(context.Background(), w, reportJSON(t, c)) }()
	<-f.inRec
	if perr := l.handle(t.Context(), w, reportJSON(t, c)); perr == nil || perr.Code != proto.CodeConflict {
		t.Errorf("repeat during the record answered %v, want conflict", perr)
	}
	close(f.gate)
	if perr := <-first; perr != nil {
		t.Fatal(perr)
	}
	f.inRec = nil
	if perr := l.handle(t.Context(), w, reportJSON(t, c)); perr != nil {
		t.Errorf("repeat after the record: %v", perr)
	}
	if len(f.evs) != 3 {
		t.Errorf("recorded %d events, want 3", len(f.evs))
	}
	// A report without a report id is refused.
	if perr := l.handle(t.Context(), w, reportJSON(t, proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered"})); perr == nil || perr.Code != proto.CodeInvalidArgument {
		t.Errorf("report without a report id answered %v", perr)
	}
}

// The log record of each local write carries the id of that write, not the
// id of the HTTP request that opened the Worker's connection.
func TestLocalAuditLogsTheIDOfEachWrite(t *testing.T) {
	st := openStore(t, t.TempDir())
	var buf syncBuf
	al := audit.New(st, audit.WithLogger(textLogger(&buf)))
	l := NewLocalAudit(al)
	l.log = quietLogger()
	w := ops.NewUUIDv7()
	conn := audit.WithRequestID(context.Background(), "upgrade-request")
	ids := []string{ops.NewUUIDv7(), ops.NewUUIDv7()}
	for _, id := range ids {
		if perr := l.handle(conn, w, reportJSON(t, proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered", RequestID: id, ReportID: ops.NewUUIDv7()})); perr != nil {
			t.Fatal(perr)
		}
	}
	report := ops.NewUUIDv7()
	if perr := l.handle(conn, w, reportJSON(t, proto.LocalAuditArgs{Op: ops.OpSendKeys, Outcome: "delivered", ReportID: report})); perr != nil {
		t.Fatal(perr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := al.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	for _, id := range append(ids, report) {
		if strings.Count(logs, "request_id="+id) != 1 {
			t.Errorf("log lacks the write id %s once:\n%s", id, logs)
		}
	}
	if strings.Contains(logs, "upgrade-request") {
		t.Errorf("local writes logged with the connection's request id:\n%s", logs)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A103: text delivered into a busy agent is reported delivered with reason
// busy; the Hub records it and it is not a failure (not logged at WARN).
func TestLocalAuditDeliveredBusyIsNotAFailure(t *testing.T) {
	ev, err := localAuditEvent(ops.NewUUIDv7(), proto.LocalAuditArgs{Op: ops.OpSendText, Session: "mt-claude", Outcome: "delivered", Reason: "busy", ReportID: ops.NewUUIDv7()})
	if err != nil || ev.Outcome != "delivered:busy" {
		t.Fatalf("event %+v, err %v", ev, err)
	}
	if audit.BadOutcome(ev.Outcome) {
		t.Fatal("delivered:busy counts as a failure")
	}
}
