package cli

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A settings_set batch that carries a secret value is refused on the command
// line and accepted from standard input.
func TestBatchWithASecretNeedsStandardInput(t *testing.T) {
	hub := &fakeOpsBackend{call: answerWith(writeResult(ops.OutcomeDelivered, "", false), nil)}
	deps := testDeps(&fakeOpsBackend{})
	deps.hub = func() (client.Backend, error) { return hub, nil }
	batch := `[{"key":"otel_endpoint","value":"https://c.example"},{"key":"otel_headers","value":"Authorization: Bearer s3cret"}]`
	code, _, errs := runWith(t, deps, "", "settings_set", "--changes", batch)
	if code != ExitUsage || !strings.Contains(errs, "standard input") || strings.Contains(errs, "s3cret") || hub.count() != 0 {
		t.Fatalf("exit %d, stderr %q, calls %d", code, errs, hub.count())
	}
	if code, _, errs := runWith(t, deps, batch, "settings_set", "--changes", "-"); code != 0 {
		t.Fatalf("stdin: exit %d: %s", code, errs)
	}
	// A batch of plain settings may be a literal; removing a secret too.
	if code, _, errs := runWith(t, deps, "", "settings_set", "--changes", `[{"key":"otel","value":"on"},{"key":"otel_headers","unset":true}]`); code != 0 {
		t.Fatalf("plain: exit %d: %s", code, errs)
	}
}
