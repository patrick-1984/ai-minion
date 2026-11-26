package localapi

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestParseRequestKeepsOnlyAValidClientRule(t *testing.T) {
	id := ops.NewUUIDv7()
	rq, perr := parseRequest([]byte(`{"type":"op.request","op":"send_text","args":{},"client_rule":"` + id + `"}`))
	if perr != nil || rq.ClientRule != id {
		t.Fatalf("rq = %+v, %v", rq, perr)
	}
	rq, perr = parseRequest([]byte(`{"type":"op.request","op":"send_text","args":{},"client_rule":"nope"}`))
	if perr != nil || rq.ClientRule != "" {
		t.Fatalf("rq = %+v, %v", rq, perr)
	}
}
