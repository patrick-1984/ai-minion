package workers

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestLocalAuditEvent(t *testing.T) {
	w := ops.NewUUIDv7()
	ev, err := localAuditEvent(w, proto.LocalAuditArgs{Op: ops.OpSendText, Session: "mt-claude", Outcome: "delivered", SourceSession: "wb-1"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Actor != "worker:"+w+"/session:wb-1" || ev.Action != "local.send_text" || ev.Target != w+":mt-claude" || ev.Outcome != "delivered" {
		t.Errorf("event = %+v", ev)
	}
	ev, _ = localAuditEvent(w, proto.LocalAuditArgs{Op: ops.OpSendKeys, Outcome: "refused", Reason: "forbidden"})
	if ev.Actor != "worker:"+w+"/local" || ev.Outcome != "refused:forbidden" || ev.Target != w {
		t.Errorf("event = %+v", ev)
	}
	for _, bad := range []string{"sessions_list", "settings_set", "worker_remove", "nope"} {
		if _, err := localAuditEvent(w, proto.LocalAuditArgs{Op: bad, Outcome: "x"}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := localAuditEvent(w, proto.LocalAuditArgs{Op: ops.OpSendText, Session: strings.Repeat("x", 600)}); err == nil {
		t.Error("accepted an overlong session")
	}
}
