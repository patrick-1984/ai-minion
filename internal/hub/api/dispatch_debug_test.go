package api

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// A refusal and a served request each leave one debug line with the
// operation, the stage or outcome and the code, never the arguments.
func TestDispatcherDebugLines(t *testing.T) {
	r := newRig(t)
	r.decide.fn = func(decideCall) error { return proto.NewError(proto.CodeForbidden, "no") }
	mustHandle(t, r.reg, opPing, echoHandler)
	r.do("POST", "/api/v1/ops/"+opPing, `{"message":"private words"}`)
	got := r.logs.String()
	for _, want := range []string{"api request refused", "op=" + opPing, "stage=authorize", "principal_kind=browser_session", "code=forbidden"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}

	r = newRig(t)
	mustHandle(t, r.reg, opPing, echoHandler)
	r.do("POST", "/api/v1/ops/"+opPing, `{"message":"private words"}`)
	got = r.logs.String()
	for _, want := range []string{"api request done", "op=" + opPing, "outcome=ok", "duration="} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "private words") {
		t.Errorf("the arguments reached the log: %q", got)
	}
}
