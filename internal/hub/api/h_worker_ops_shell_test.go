package api

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// woShellRouter reports a command shell grant for every request it routes,
// as the Router does for a write into a command shell (A116).
type woShellRouter struct {
	woRouter
	grant string
}

func (r *woShellRouter) Call(ctx context.Context, req workers.CallRequest) (any, error) {
	if req.CommandShell != nil {
		req.CommandShell("") // the first decision found none
		req.CommandShell(r.grant)
	}
	return r.woRouter.Call(ctx, req)
}

// A REST write into a command shell names the grant in its audit row the way
// the call stream does: command_shell=<grant> in the detail, and the grant as
// the row's grant when the handler named none. A read asks for no report.
func TestWorkerOpRESTAuditNamesTheCommandShell(t *testing.T) {
	grant := ops.NewUUIDv7()
	wid := ops.NewUUIDv7()
	rt := &woShellRouter{woRouter: woRouter{callRes: ops.WriteResult{Outcome: ops.OutcomeDelivered}}, grant: grant}
	reg := NewRegistry(ops.Default)
	mustRegister(t, RegisterWorkerOps(reg, rt))
	run := func(name string, c *Call) {
		t.Helper()
		e, ok := reg.lookup(name)
		if !ok || e.handler == nil {
			t.Fatalf("%s has no handler", name)
		}
		if _, err := e.handler(context.Background(), c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	res := authz.SessionResource{WorkerID: wid, Name: "shell"}
	c := &Call{Resource: res, AuditDetail: "client_rule=x"}
	run(ops.OpSendText, c)
	if c.AuditDetail != "client_rule=x command_shell="+grant || c.AuditGrantID != grant {
		t.Fatalf("send_text audit: detail %q, grant %q", c.AuditDetail, c.AuditGrantID)
	}
	c = &Call{Resource: res, AuditGrantID: "other"}
	run(ops.OpSendKeys, c)
	if c.AuditDetail != "command_shell="+grant || c.AuditGrantID != "other" {
		t.Fatalf("send_keys audit: detail %q, grant %q", c.AuditDetail, c.AuditGrantID)
	}
	// No grant reported: nothing is added.
	rt.grant = ""
	c = &Call{Resource: res}
	run(ops.OpSendText, c)
	if c.AuditDetail != "" || c.AuditGrantID != "" {
		t.Fatalf("no command shell: detail %q, grant %q", c.AuditDetail, c.AuditGrantID)
	}
	c = &Call{Resource: res}
	run(ops.OpSessionSnapshot, c)
	if rt.last(t).CommandShell != nil {
		t.Fatal("a read asked for a command shell report")
	}
}
