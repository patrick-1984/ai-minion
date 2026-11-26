package workers

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The client_rule of a call frame is a label: the audit event of the write
// names a valid one, and drops anything else without refusing the call.
func TestCallStreamWriteAuditNamesTheClientRule(t *testing.T) {
	for _, c := range []struct{ rule string }{{ops.NewUUIDv7()}, {"not a uuid"}} {
		r := newCallRig(t, nil)
		req := request(ops.OpPipeSend, map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "ping"}, "")
		req["client_rule"] = c.rule
		frames := r.call(t, wAlicePi1.ID, req)
		if typ, body := lastFrame(t, frames); typ != "op.result" {
			t.Fatalf("answer = %s", body)
		}
		evs := r.audit.all()
		if len(evs) != 1 {
			t.Fatalf("audit = %+v", evs)
		}
		want := ""
		if ops.ValidUUIDv7(c.rule) {
			want = "client_rule=" + c.rule
		}
		if evs[0].Detail != want {
			t.Errorf("rule %q: detail = %q, want %q", c.rule, evs[0].Detail, want)
		}
	}
}
