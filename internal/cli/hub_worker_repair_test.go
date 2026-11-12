package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func runRepairPairing(t *testing.T, b *fakeOpsBackend, arg string) (string, error) {
	t.Helper()
	deps := opsDeps{
		hub:   func() (client.Backend, error) { return b, nil },
		newID: func() string { return "0190a1b2-c3d4-7e5f-8a6b-0000000000bb" },
	}
	var out bytes.Buffer
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newHubWorkerRepairPairing(deps))
	root.SetArgs([]string{"repair-pairing", arg})
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func repairBackend(answer error, workers ...ops.WorkerInfo) *fakeOpsBackend {
	return &fakeOpsBackend{call: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		switch op {
		case ops.OpWorkersList:
			return json.Marshal(ops.WorkersListResult{Workers: workers})
		case ops.OpWorkerPairingReissue:
			if answer != nil {
				return nil, answer
			}
			return json.Marshal(ops.WriteResult{Outcome: ops.OutcomeDelivered})
		}
		return nil, proto.NewError(proto.CodeUnsupportedOp, op)
	}}
}

func TestHubWorkerRepairPairingIsRegistered(t *testing.T) {
	cmd, _, err := NewRoot().Find([]string{"hub", "worker", "repair-pairing"})
	if err != nil || cmd.Name() != "repair-pairing" {
		t.Fatalf("hub worker repair-pairing not found: %v", err)
	}
}

func TestHubWorkerRepairPairingByIDAndByFingerprint(t *testing.T) {
	fp := strings.Repeat("e", 52)
	w := ops.WorkerInfo{ID: "0190a1b2-c3d4-7e5f-8a6b-000000000011", Name: "edge", Fingerprint: fp, NeverConnected: true}
	for _, ref := range []string{w.ID, fp} {
		b := repairBackend(nil, w)
		out, err := runRepairPairing(t, b, ref)
		if err != nil {
			t.Fatalf("repair-pairing %s: %v\n%s", ref, err, out)
		}
		if !strings.Contains(out, "Pairing re-issued for Worker "+w.ID) || !strings.Contains(out, "minion worker pair") {
			t.Errorf("output = %q", out)
		}
		last := b.last(t)
		var a ops.WorkerPairingReissueArgs
		if last.Op != ops.OpWorkerPairingReissue || json.Unmarshal(last.Args, &a) != nil || a.WorkerID != w.ID || a.RequestID == "" {
			t.Errorf("call = %s %s", last.Op, last.Args)
		}
	}
}

func TestHubWorkerRepairPairingReportsConflictAndBadArguments(t *testing.T) {
	b := repairBackend(proto.NewError(proto.CodeConflict, "this Worker cannot be re-paired"))
	out, err := runRepairPairing(t, b, "0190a1b2-c3d4-7e5f-8a6b-000000000012")
	if err == nil || !strings.Contains(out+err.Error(), "re-paired") {
		t.Errorf("conflict: err = %v, out = %q", err, out)
	}
	if _, err := runRepairPairing(t, repairBackend(nil), "edge"); err == nil {
		t.Error("a name that is neither an id nor a fingerprint was accepted")
	}
	if _, err := runRepairPairing(t, repairBackend(nil), strings.Repeat("f", 52)); err == nil {
		t.Error("an unknown fingerprint was accepted")
	}
}
