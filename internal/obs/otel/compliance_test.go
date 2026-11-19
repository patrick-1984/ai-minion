package otel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
)

func newComplianceForTest(t *testing.T, url string, headers map[string]string, opts ComplianceOptions) *Compliance {
	t.Helper()
	InstallGlobal(NewDiag(slog.NewTextHandler(io.Discard, nil)))
	cfg := config.ComplianceConfig{Level: "output", LogsURL: url + "/v1/logs", Headers: headers}
	c, err := newCompliance(context.Background(), cfg, Process{Service: "minion-hub", Version: "v0.15.0", InstanceID: "hub-1"}, opts, retry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Discard)
	return c
}

func rec(event, body string, attrs ...compliance.Attr) compliance.Record {
	return compliance.Record{Event: event, Time: time.Now(), Body: body, Attrs: attrs}
}

// The records reach the collector on the exact path with the configured
// headers, with the event name, the attributes, the body and the resource.
func TestComplianceRecordsReachTheCollector(t *testing.T) {
	rcv := newReceiver(t)
	c := newComplianceForTest(t, rcv.srv.URL, map[string]string{"Authorization": "Bearer s3cr3t-token"}, ComplianceOptions{Interval: 20 * time.Millisecond})
	c.Emit(rec(compliance.EventPolicy, "", compliance.Attr{Key: "minion.policy.version", Value: int64(3)}))
	c.Emit(rec(compliance.EventInput, "echo hello", compliance.Attr{Key: "minion.input.status", Value: "attempted"},
		compliance.Attr{Key: "minion.turn.ids", Value: []string{"a", "b"}}, compliance.Attr{Key: "minion.input.submit", Value: true}))
	eventually(t, func() bool { return c.Stats().Sent == 2 })
	reqs := rcv.requests()
	if len(reqs) == 0 || reqs[0].path != "/v1/logs" || reqs[0].headers.Get("Authorization") != "Bearer s3cr3t-token" {
		t.Fatalf("requests = %+v", reqs)
	}
	var names []string
	var body string
	attrs := map[string]string{}
	res := map[string]string{}
	for _, g := range reqs {
		for _, rl := range g.req.ResourceLogs {
			for _, kv := range rl.GetResource().GetAttributes() {
				res[kv.Key] = kv.Value.GetStringValue()
			}
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					names = append(names, lr.EventName)
					if lr.EventName == compliance.EventInput {
						body = lr.Body.GetStringValue()
						for _, kv := range lr.Attributes {
							attrs[kv.Key] = kv.Value.String()
						}
					}
				}
			}
		}
	}
	if strings.Join(names, ",") != compliance.EventPolicy+","+compliance.EventInput || body != "echo hello" {
		t.Fatalf("events %v body %q", names, body)
	}
	if res["service.name"] != "minion-hub" || res["service.instance.id"] != "hub-1" {
		t.Fatalf("resource = %v", res)
	}
	if !strings.Contains(attrs["minion.input.status"], "attempted") || !strings.Contains(attrs["minion.turn.ids"], "a") {
		t.Fatalf("attrs = %v", attrs)
	}
	// Nothing of the credential is in the status.
	if st := c.Status(); strings.Contains(fmt.Sprint(st), "s3cr3t") || st.Destination == "" || strings.Contains(st.Destination, "/v1") {
		t.Fatalf("status = %+v", st)
	}
}

// slowReceiver answers only when released; it counts the requests.
type slowReceiver struct {
	*receiver
	gate chan struct{}
	hits atomic.Int64
}

func newSlowReceiver(t *testing.T) *slowReceiver {
	s := &slowReceiver{receiver: &receiver{}, gate: make(chan struct{})}
	inner := newReceiver(t)
	s.receiver = inner
	h := inner.srv.Config.Handler
	inner.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		<-s.gate
		h.ServeHTTP(w, r)
	})
	return s
}

func (s *slowReceiver) release() {
	select {
	case <-s.gate:
	default:
		close(s.gate)
	}
}

// Flood with a slow receiver: every record is accounted for (emitted = sent +
// dropped + queued), what is queued keeps its order, the loss is reported to
// the collector as a gap record that names the events, and no request is
// larger than its budget.
func TestComplianceFloodAccountingGapAndRequestBudget(t *testing.T) {
	slow := newSlowReceiver(t)
	t.Cleanup(slow.release)
	opts := ComplianceOptions{QueueRecords: 40, QueueBytes: 1 << 20, RequestRecords: 8, RequestBytes: 64 << 10, Interval: 10 * time.Millisecond}
	c := newComplianceForTest(t, slow.srv.URL, nil, opts)
	big := strings.Repeat("x", 4<<10)
	const total = 500
	for i := 0; i < total; i++ {
		ev := compliance.EventAction
		if i%3 == 0 {
			ev = compliance.EventOutput
		}
		c.Emit(rec(ev, big, compliance.Attr{Key: "minion.seq", Value: int64(i)}))
	}
	st := c.Stats()
	if st.Emitted != total {
		t.Fatalf("emitted = %d", st.Emitted)
	}
	if st.Dropped[DropQueueFull] == 0 || st.Queued > 40+8 {
		t.Fatalf("queue not bounded: %+v", st)
	}
	if st.Emitted != st.Sent+st.Failed+st.Queued+st.Dropped[DropQueueFull]+st.Dropped[DropReconfigure]+st.Dropped[DropShutdown] {
		t.Fatalf("accounting does not add up: %+v", st)
	}
	slow.release()
	eventually(t, func() bool {
		s := c.Stats()
		return s.Queued == 0 && s.Sent+s.Dropped[DropQueueFull] >= total
	})
	st = c.Stats()
	// The gap record is one more emitted and sent record.
	if st.Emitted != st.Sent+st.Dropped[DropQueueFull] || st.Failed != 0 {
		t.Fatalf("accounting after flush: %+v", st)
	}
	if st.MaxRequestRecords > opts.RequestRecords || st.MaxRequestBytes > opts.RequestBytes {
		t.Fatalf("a request exceeded its budget: %d records %d bytes", st.MaxRequestRecords, st.MaxRequestBytes)
	}
	// The collector saw the gap, with the count and the events.
	var gaps, kept int
	var gapN int64
	var seq []int64
	for _, g := range slow.requests() {
		for _, rl := range g.req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					switch lr.EventName {
					case compliance.EventGap:
						gaps++
						for _, kv := range lr.Attributes {
							if kv.Key == "minion.dropped" {
								gapN += kv.Value.GetIntValue()
							}
							if kv.Key == "minion.dropped.by_event" && !strings.Contains(kv.Value.GetStringValue(), "minion.session.") {
								t.Fatalf("by_event = %q", kv.Value.GetStringValue())
							}
						}
					default:
						kept++
						for _, kv := range lr.Attributes {
							if kv.Key == "minion.seq" {
								seq = append(seq, kv.Value.GetIntValue())
							}
						}
					}
				}
			}
		}
	}
	if gaps == 0 || uint64(gapN) != st.Dropped[DropQueueFull] || uint64(kept) != st.Sent-uint64(gaps) {
		t.Fatalf("gaps %d (dropped %d vs %d), kept %d, sent %d", gaps, gapN, st.Dropped[DropQueueFull], kept, st.Sent)
	}
	for i := 1; i < len(seq); i++ {
		if seq[i] <= seq[i-1] {
			t.Fatalf("the order of the queued records changed: %v", seq)
		}
	}
	if len(slow.requests()) < 2 {
		t.Fatal("the records must be split into several requests")
	}
}

// The byte budget alone also drops new records, and an estimated request
// never exceeds its budget; the budgets sit far below a collector's default
// request limit of 20 MiB.
func TestComplianceByteBudgetsAndDefaults(t *testing.T) {
	if ComplianceRequestBytes*4 > 20<<20 || ComplianceQueueBytes > 20<<20 {
		t.Fatal("the budgets must stay far below the collector's 20 MiB request limit")
	}
	if ComplianceQueueRecords != 4096 || ComplianceQueueBytes != 16<<20 || ComplianceRequestRecords != 256 || ComplianceRequestBytes != 4<<20 {
		t.Fatal("the budgets of design §5.8 changed")
	}
	slow := newSlowReceiver(t)
	t.Cleanup(slow.release)
	c := newComplianceForTest(t, slow.srv.URL, nil, ComplianceOptions{QueueRecords: 1000, QueueBytes: 50 << 10, RequestRecords: 100, RequestBytes: 20 << 10, Interval: time.Hour})
	for i := 0; i < 30; i++ {
		c.Emit(rec(compliance.EventOutput, strings.Repeat("y", 10<<10)))
	}
	st := c.Stats()
	if st.QueuedBytes > 50<<10 || st.Dropped[DropQueueFull] == 0 {
		t.Fatalf("byte budget not enforced: %+v", st)
	}
	if st.DroppedByEvent[compliance.EventOutput] != st.Dropped[DropQueueFull] {
		t.Fatalf("drops by event = %v", st.DroppedByEvent)
	}
}

// Records queued under a revoked destination are never sent: Discard drops
// them, counted as reconfigure, and the old collector gets nothing later.
func TestComplianceDiscardNeverSendsTheQueue(t *testing.T) {
	slow := newSlowReceiver(t)
	c := newComplianceForTest(t, slow.srv.URL, nil, ComplianceOptions{Interval: time.Hour})
	for i := 0; i < 10; i++ {
		c.Emit(rec(compliance.EventAction, "queued"))
	}
	if st := c.Stats(); st.Queued != 10 {
		t.Fatalf("queued = %d", st.Queued)
	}
	c.Discard()
	slow.release()
	time.Sleep(100 * time.Millisecond)
	if st := c.Stats(); st.Dropped[DropReconfigure] != 10 || st.Sent != 0 || len(slow.requests()) != 0 {
		t.Fatalf("stats %+v requests %d", st, len(slow.requests()))
	}
	// A late Emit after the end is counted, not queued.
	c.Emit(rec(compliance.EventAction, "late"))
	if st := c.Stats(); st.Queued != 0 {
		t.Fatalf("queued after discard = %d", st.Queued)
	}
}

// A dead endpoint: Close returns within its deadline, the rest is dropped as
// shutdown, and the local status carries a sanitised error.
func TestComplianceCloseWithDeadEndpointIsBounded(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c := newComplianceForTest(t, url, map[string]string{"Authorization": "Bearer hush"}, ComplianceOptions{Interval: 10 * time.Millisecond})
	for i := 0; i < 20; i++ {
		c.Emit(rec(compliance.EventAction, "x"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = c.Close(ctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close took %v", d)
	}
	st := c.Stats()
	if st.Emitted != st.Sent+st.Failed+st.Queued+st.Dropped[DropShutdown]+st.Dropped[DropReconfigure]+st.Dropped[DropQueueFull] || st.Sent != 0 {
		t.Fatalf("accounting: %+v", st)
	}
	if strings.Contains(st.LastError, "hush") || strings.Contains(c.Status().LastError, "Bearer") {
		t.Fatalf("a credential leaked into the status: %q", st.LastError)
	}
}

// An export that fails is counted as failed with a local, sanitised error.
func TestComplianceExportFailureIsCountedAndSanitised(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.Error(w, "bad token Bearer s3cr3t from collector", http.StatusUnauthorized)
	}))
	defer srv.Close()
	InstallGlobal(NewDiag(slog.NewTextHandler(io.Discard, nil)))
	cfg := config.ComplianceConfig{Level: "actions", LogsURL: srv.URL + "/v1/logs", Headers: map[string]string{"Authorization": "Bearer s3cr3t"}}
	c, err := newCompliance(context.Background(), cfg, Process{Service: "minion-hub"}, ComplianceOptions{Interval: 10 * time.Millisecond}, retry)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Discard()
	c.Emit(rec(compliance.EventAction, ""))
	eventually(t, func() bool { return c.Stats().Failed == 1 })
	st := c.Stats()
	if !strings.Contains(st.LastError, "HTTP 401") || strings.Contains(st.LastError, "s3cr3t") || st.Dropped[DropExportFailed] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// End to end: content that holds Minion's own secret formats leaves the
// process masked, whatever its path (input, output, turn text, task), and
// nothing else of it is changed.
func TestNoMinionSecretLeavesInAnyRecord(t *testing.T) {
	rcv := newReceiver(t)
	c := newComplianceForTest(t, rcv.srv.URL, nil, ComplianceOptions{Interval: 10 * time.Millisecond})
	apiKey := "mn_" + strings.Repeat("q", 52)
	invite := "mnk_" + strings.Repeat("a", 10) + "." + strings.Repeat("z", 20)
	join := "mnj1-" + strings.Repeat("Ab3_-", 10)
	pairing := "MN-7K2P-9QXA"
	secrets := []string{apiKey, strings.Repeat("z", 20), join, "7K2P-9QXA"}
	text := fmt.Sprintf("keys %s %s %s %s done", apiKey, invite, join, pairing)

	m := compliance.NewMonitor(compliance.Deps{
		UserManaged: func(context.Context, string) (bool, error) { return true, nil },
	})
	m.SetSink(c)
	m.SetPolicy(compliance.Policy{Version: 1, Level: compliance.LevelOutput, Input: true})
	info := func(op string, args any) compliance.CallInfo {
		return compliance.CallInfo{Path: compliance.PathREST, Op: op, Args: args, Client: compliance.ClientAPI, PrincipalKind: "api_key", Actor: "key:k"}
	}
	tgt := opsTarget()
	for _, a := range []struct {
		op   string
		args any
	}{
		{"send_text", sendTextArgs(tgt, text)},
		{"set_task", setTaskArgs(tgt, text)},
		{"pipe_send", pipeSendArgs(tgt, text)},
		{"answer", answerArgs(tgt, text)},
	} {
		cp := m.Begin(context.Background(), info(a.op, a.args))
		cp.Attempted()
		cp.Done(okResult(), "")
	}
	m.Begin(context.Background(), info("session_snapshot", snapshotArgs(tgt))).Done(snapshotResult(text), "")
	m.Begin(context.Background(), info("pipe_read", pipeReadArgs(tgt))).Done(pipeReadResult(text), "")

	eventually(t, func() bool { s := c.Stats(); return s.Queued == 0 && s.Sent >= 10 })
	var seen int
	for _, g := range rcv.requests() {
		raw := fmt.Sprint(g.req)
		for _, s := range secrets {
			if strings.Contains(raw, s) {
				t.Fatalf("a secret left in a record: %q", s)
			}
		}
		seen += strings.Count(raw, "mn_[redacted]")
		if !strings.Contains(raw, "keys ") && strings.Contains(raw, "done") {
			t.Fatal("masking changed the text around the secrets")
		}
	}
	if seen < 6 {
		t.Fatalf("redaction marker found %d times", seen)
	}
}
