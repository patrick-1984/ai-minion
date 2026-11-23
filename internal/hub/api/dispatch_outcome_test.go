package api

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// A worker_update the Worker did not start is audited as a failure.
func TestWorkerUpdateAuditOutcome(t *testing.T) {
	if got := outcomeOf(ops.WorkerUpdateResult{Accepted: true}); got != AuditOutcomeSuccess {
		t.Errorf("accepted = %q", got)
	}
	if got := outcomeOf(ops.WorkerUpdateResult{Reason: "x"}); got != auditOutcomeError {
		t.Errorf("refused = %q", got)
	}
	if got := outcomeOf(&ops.WorkerUpdateResult{Reason: "x"}); got != auditOutcomeError {
		t.Errorf("refused pointer = %q", got)
	}
}
