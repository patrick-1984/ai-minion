package agent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestDecodeReply(t *testing.T) {
	w, err := decodeReply(json.RawMessage(`{"type":"welcome","worker_id":"w-1","name":"pi","owner":"alice","kind":"infrastructure",` +
		`"policy":{"preview_only":true},"proto_min":"1.0","proto_max":"1.1","future_field":42}`))
	if err != nil || w.Type != proto.TypeWelcome {
		t.Fatalf("welcome: %+v, %v", w, err)
	}
	if h := w.Welcome; h.WorkerID != "w-1" || h.Owner != "alice" || h.Kind != "infrastructure" || !h.Policy.PreviewOnly ||
		h.Max != (proto.Version{Major: 1, Minor: 1}) {
		t.Fatalf("welcome fields = %+v", h)
	}

	r, err := decodeReply(json.RawMessage(`{"type":"revoke","sig":"AQID"}`))
	if err != nil || r.Type != proto.TypeRevoke || len(r.RevokeSig) != 3 {
		t.Fatalf("revoke: %+v, %v", r, err)
	}
	// A revoke without a usable signature is still reported as a revoke: the
	// connector ignores it, because an empty signature never verifies.
	r, err = decodeReply(json.RawMessage(`{"type":"revoke"}`))
	if err != nil || r.Type != proto.TypeRevoke || len(r.RevokeSig) != 0 {
		t.Fatalf("unsigned revoke: %+v, %v", r, err)
	}
	r, err = decodeReply(json.RawMessage(`{"type":"revoke","sig":"!!!"}`))
	if err != nil || r.Type != proto.TypeRevoke || len(r.RevokeSig) != 0 {
		t.Fatalf("garbled revoke: %+v, %v", r, err)
	}

	n, err := decodeReply(json.RawMessage(`{"type":"not_enrolled"}`))
	if err != nil || n.Type != proto.TypeNotEnrolled {
		t.Fatalf("not_enrolled: %+v, %v", n, err)
	}

	_, err = decodeReply(json.RawMessage(`{"type":"error","code":"rate_limited","message":"slow down"}`))
	if !errors.Is(err, proto.NewError(proto.CodeRateLimited, "")) {
		t.Fatalf("error frame: %v", err)
	}

	for _, bad := range []string{`{"type":"hello"}`, `{}`, `not json`, `{"type":"welcome","worker_id":5}`} {
		if _, err := decodeReply(json.RawMessage(bad)); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}
