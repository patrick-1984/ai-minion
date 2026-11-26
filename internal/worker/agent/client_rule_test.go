package agent

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A forwarded call names the client rule of its local request, and an invalid
// one names none.
func TestCallFrameCarriesTheClientRule(t *testing.T) {
	c := NewCallClient(CallConfig{})
	rule := ops.NewUUIDv7()
	f, err := c.request(WithClientRule(context.Background(), rule), ops.OpSendText, map[string]any{})
	if err != nil || f.ClientRule != rule {
		t.Fatalf("frame = %+v, %v", f, err)
	}
	f, err = c.request(WithClientRule(context.Background(), "bogus"), ops.OpSendText, map[string]any{})
	if err != nil || f.ClientRule != "" {
		t.Fatalf("frame = %+v, %v", f, err)
	}
}
