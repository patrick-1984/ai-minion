package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The Minion-Client-Rule header of a write is recorded in the audit detail
// when it is a UUIDv7, and ignored otherwise; it never changes the answer.
func TestWriteAuditNamesTheClientRuleHeader(t *testing.T) {
	rule := ops.NewUUIDv7()
	for _, c := range []struct{ header, want string }{
		{rule, "client_rule=" + rule},
		{"x y", ""},
		{"", ""},
	} {
		r := newRig(t)
		mustHandle(t, r.reg, opPingSet, func(context.Context, *Call) (any, error) {
			return ops.WriteResult{Outcome: ops.OutcomeDelivered}, nil
		})
		req := httptest.NewRequest("POST", "/api/v1/ops/ping_set", strings.NewReader(`{"request_id":"`+ops.NewUUIDv7()+`","message":"m"}`))
		if c.header != "" {
			req.Header.Set(proto.ClientRuleHeader, c.header)
		}
		w := httptest.NewRecorder()
		r.mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		ev := r.audit.all()
		if len(ev) != 1 || ev[0].Detail != c.want {
			t.Errorf("header %q: audit = %+v, want detail %q", c.header, ev, c.want)
		}
	}
}
