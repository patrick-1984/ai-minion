package workers

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestLocalAuditEvent(t *testing.T) {
	w := ops.NewUUIDv7()
	ev, err := localAuditEvent(w, proto.LocalAuditArgs{Op: ops.OpSendText, Session: "mt-claude", Outcome: "delivered", SourceSession: "wb-1"})
	if err != nil {
		t.Fatal(err)
	}
	// The calling session a Worker names is not verified: the actor is the
	// authenticated Worker's local entry point.
	if ev.Actor != "worker:"+w+"/local" || ev.Action != "local.send_text" || ev.Target != w+":mt-claude" || ev.Outcome != "delivered" {
		t.Errorf("event = %+v", ev)
	}
	ev, _ = localAuditEvent(w, proto.LocalAuditArgs{Op: ops.OpSendKeys, Outcome: "refused", Reason: "forbidden"})
	if ev.Actor != "worker:"+w+"/local" || ev.Outcome != "refused:forbidden" || ev.Target != w {
		t.Errorf("event = %+v", ev)
	}
	for _, bad := range []string{"sessions_list", "settings_set", "worker_remove", "nope"} {
		if _, err := localAuditEvent(w, proto.LocalAuditArgs{Op: bad, Outcome: "delivered"}); err == nil {
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
	a := proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered", RequestID: id}
	if l.admit(w, a) != admitOK || l.admit(w, a) != admitDuplicate {
		t.Fatal("a repeated report of the same write was not deduplicated")
	}
	n := 1
	for ; n < 1000; n++ {
		if l.admit(w, proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered"}) == admitDropped {
			break
		}
	}
	if n != localAuditBurst {
		t.Errorf("admitted %d reports before the first drop, want %d", n, localAuditBurst)
	}
	if l.Dropped(w) != 1 {
		t.Errorf("dropped = %d", l.Dropped(w))
	}
	// Another Worker has its own budget; time refills it.
	if l.admit(other, proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered"}) != admitOK {
		t.Error("another Worker was throttled")
	}
	now = now.Add(5 * time.Second)
	if l.admit(w, proto.LocalAuditArgs{Op: ops.OpSendText, Outcome: "delivered"}) != admitOK {
		t.Error("the budget did not refill")
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
