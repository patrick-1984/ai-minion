package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func runApprove(t *testing.T, b *fakeOpsBackend, arg string) (string, error) {
	t.Helper()
	deps := opsDeps{
		hub:   func() (client.Backend, error) { return b, nil },
		newID: func() string { return "0190a1b2-c3d4-7e5f-8a6b-0000000000aa" },
	}
	cmd := newHubWorkerApprove(deps)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{arg})
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(cmd)
	root.SetArgs([]string{"approve", arg})
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func approveBackend(workers ...ops.WorkerInfo) *fakeOpsBackend {
	return &fakeOpsBackend{call: func(op string, args json.RawMessage) (json.RawMessage, error) {
		switch op {
		case ops.OpWorkersList:
			return json.Marshal(ops.WorkersListResult{Workers: workers})
		case ops.OpWorkerApprove:
			return json.Marshal(ops.WriteResult{Outcome: ops.OutcomeDelivered})
		}
		return nil, proto.NewError(proto.CodeUnsupportedOp, op)
	}}
}

func TestHubWorkerApproveByFingerprintAndByID(t *testing.T) {
	fp := strings.Repeat("c", 52)
	w := ops.WorkerInfo{ID: "0190a1b2-c3d4-7e5f-8a6b-000000000001", Name: "edge", Pending: true, Fingerprint: fp, EnrollAddr: "203.0.113.5"}
	other := ops.WorkerInfo{ID: "0190a1b2-c3d4-7e5f-8a6b-000000000002", Name: "other", Pending: true, Fingerprint: strings.Repeat("d", 52)}
	for _, ref := range []string{fp, w.ID} {
		b := approveBackend(other, w)
		out, err := runApprove(t, b, ref)
		if err != nil {
			t.Fatalf("approve %s: %v\n%s", ref, err, out)
		}
		wantFP := ref == fp
		if wantFP && (!strings.Contains(out, "Approved Worker edge") || !strings.Contains(out, "203.0.113.5") || !strings.Contains(out, fp)) {
			t.Errorf("output = %q", out)
		}
		if !wantFP && !strings.Contains(out, "Approved Worker "+w.ID) {
			t.Errorf("output = %q", out)
		}
		last := b.last(t)
		var a ops.WorkerApproveArgs
		if last.Op != ops.OpWorkerApprove || json.Unmarshal(last.Args, &a) != nil || a.WorkerID != w.ID || (wantFP && a.Fingerprint != fp) || a.RequestID == "" {
			t.Errorf("approve call = %s %s", last.Op, last.Args)
		}
	}
}

type approveLookups struct{ wid string }

func (l approveLookups) User(_ context.Context, id string) (authz.UserState, error) {
	if id == "admin" {
		return authz.UserState{Role: authz.RoleAdmin, Active: true}, nil
	}
	return authz.UserState{}, authz.ErrNotFound
}

func (l approveLookups) WorkerOwner(_ context.Context, id string) (string, error) {
	if id == l.wid {
		return "admin", nil
	}
	return "", authz.ErrNotFound
}

// manageOnly is the scope of an admin API key that holds manage and nothing else.
type manageOnly struct{}

func (manageOnly) Actions(authz.ResolvedResource) []ops.Action { return []ops.Action{ops.ActionManage} }

// TestHubWorkerApproveByIDNeedsOnlyManage runs the CLI against a backend that
// decides with the production authorizer and the catalog's accepted actions:
// a manage-only admin key approves by id (no workers_list call), and the
// fingerprint form, which needs the list, is refused for lack of read.
func TestHubWorkerApproveByIDNeedsOnlyManage(t *testing.T) {
	wid := "0190a1b2-c3d4-7e5f-8a6b-000000000001"
	fp := strings.Repeat("c", 52)
	az := authz.New(approveLookups{wid}, approveLookups{wid})
	p := authz.NewAPIKeyPrincipal("admin", manageOnly{}, "")
	b := &fakeOpsBackend{call: func(op string, args json.RawMessage) (json.RawMessage, error) {
		def, ok := ops.Lookup(op)
		if !ok {
			return nil, proto.NewError(proto.CodeUnsupportedOp, op)
		}
		if err := az.Decide(context.Background(), p, def.Actions, authz.WorkerResource{ID: wid}); err != nil {
			return nil, proto.NewError(proto.CodeForbidden, err.Error())
		}
		switch op {
		case ops.OpWorkersList:
			return json.Marshal(ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: wid, Name: "edge", Pending: true, Fingerprint: fp}}})
		case ops.OpWorkerApprove:
			return json.Marshal(ops.WriteResult{Outcome: ops.OutcomeDelivered})
		}
		return nil, proto.NewError(proto.CodeUnsupportedOp, op)
	}}
	out, err := runApprove(t, b, wid)
	if err != nil || !strings.Contains(out, "Approved Worker "+wid) {
		t.Fatalf("approve by id with a manage-only key: %v %q", err, out)
	}
	for _, c := range b.calls {
		if c.Op == ops.OpWorkersList {
			t.Error("approve by id called workers_list")
		}
	}
	if _, err := runApprove(t, b, fp); err == nil || !strings.Contains(err.Error(), "read action") {
		t.Errorf("approve by fingerprint with a manage-only key: %v", err)
	}
}

func TestHubWorkerApproveRefusals(t *testing.T) {
	fp := strings.Repeat("c", 52)
	approved := ops.WorkerInfo{ID: "0190a1b2-c3d4-7e5f-8a6b-000000000001", Name: "edge", Fingerprint: fp}
	b := approveBackend(approved)
	if out, err := runApprove(t, b, fp); err != nil || !strings.Contains(out, "already approved") {
		t.Errorf("approved Worker: %v %q", err, out)
	}
	for _, c := range b.calls {
		if c.Op == ops.OpWorkerApprove {
			t.Error("worker_approve was called for an approved Worker")
		}
	}
	if _, err := runApprove(t, approveBackend(), fp); err == nil || !strings.Contains(err.Error(), "no Worker") {
		t.Errorf("unknown fingerprint: %v", err)
	}
	for _, bad := range []string{"not-a-fingerprint", strings.ToUpper(fp), fp[:51]} {
		if _, err := runApprove(t, approveBackend(approved), bad); err == nil || !strings.Contains(err.Error(), "neither a Worker id") {
			t.Errorf("malformed %q: %v", bad, err)
		}
	}
	// The Hub's refusal is shown with its code.
	deny := &fakeOpsBackend{call: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		if op == ops.OpWorkersList {
			return json.Marshal(ops.WorkersListResult{Workers: []ops.WorkerInfo{{ID: approved.ID, Name: "edge", Pending: true, Fingerprint: fp}}})
		}
		return nil, proto.NewError(proto.CodeForbidden, "not allowed")
	}}
	if _, err := runApprove(t, deny, fp); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("denied: %v", err)
	}
}
