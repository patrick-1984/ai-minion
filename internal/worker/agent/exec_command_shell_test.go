package agent

import (
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// A116: a Hub request whose origin carries shell_ok skips the shell_pane
// guard of send_text and send_keys, and only that guard; a local caller
// never does.

const shellGrantID = "01a00000-0000-7000-8000-0000000000aa"

func shellOrigin(ok bool) *proto.Origin {
	return &proto.Origin{Kind: proto.OriginWorker, SourceWorkerID: ops.NewUUIDv7(), SourceWorkerFP: "fp", SourceWorkerOwnerID: "alice",
		Grant: true, GrantID: shellGrantID, SourceSession: "master", SourceProof: proto.OriginProofClaimed, ShellOK: ok}
}

func shellWrite(t *testing.T, e *env, origin *proto.Origin, op string, force bool) ops.WriteResult {
	t.Helper()
	extra := map[string]any{"text": "ls", "submit": true}
	if op == ops.OpSendKeys {
		extra = map[string]any{"keys": []string{"Enter"}}
	}
	raw, err := e.callFrom(origin, op, rawArgs("work", force, extra))
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestShellOKSkipsOnlyTheShellPaneGuard(t *testing.T) {
	for _, op := range []string{ops.OpSendText, ops.OpSendKeys} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.host.addSession("work", "bash")
			e.cls.set("bash", ops.PaneKindShell, ops.PaneStateStable)

			// Without the member, and from a local caller, the guard refuses.
			for name, origin := range map[string]*proto.Origin{"hub without shell_ok": shellOrigin(false), "local": nil} {
				res := shellWrite(t, e, origin, op, false)
				if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "shell_pane" || !res.Forceable {
					t.Fatalf("%s: %+v, want the shell_pane refusal", name, res)
				}
			}
			if n := len(e.host.eventLog()); n != 0 {
				t.Fatalf("events after refusals: %v", e.host.eventLog())
			}
			// With it, a normal delivered write: not forced.
			res := shellWrite(t, e, shellOrigin(true), op, false)
			if res.Outcome != ops.OutcomeDelivered || res.Forced || res.Reason != "" {
				t.Fatalf("command shell: %+v, want delivered and not forced", res)
			}
			if len(e.host.eventLog()) == 0 {
				t.Fatal("nothing was typed")
			}
		})
	}
}

func TestShellOKKeepsTheOtherGuards(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "bash")
	// A shell pane that shows a dialog: the dialog guard still refuses.
	e.cls.set("bash", ops.PaneKindShell, ops.PaneStateDialog)
	for _, op := range []string{ops.OpSendText, ops.OpSendKeys} {
		res := shellWrite(t, e, shellOrigin(true), op, false)
		if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != proto.CodeDialogOpen.String() {
			t.Fatalf("%s into a dialog: %+v, want dialog_open", op, res)
		}
	}
	if n := len(e.host.eventLog()); n != 0 {
		t.Fatalf("events after refusals: %v", e.host.eventLog())
	}
	// A session that is not served (not shared or gone) stays refused too.
	raw, err := e.callFrom(shellOrigin(true), ops.OpSendText, rawArgs("nope", false, map[string]any{"text": "ls"}))
	if err == nil {
		var res ops.WriteResult
		_ = json.Unmarshal(raw, &res)
		if res.Outcome == ops.OutcomeDelivered {
			t.Fatalf("a missing session was written: %+v", res)
		}
	}
}

// A Message holder's agent-only prompt never types into a shell, also with
// the member.
func TestShellOKDoesNotWidenAgentOnly(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "bash")
	e.cls.set("bash", ops.PaneKindShell, ops.PaneStateStable)
	raw, err := e.callFrom(shellOrigin(true), ops.OpSendText, rawArgs("work", false, map[string]any{"text": "ls", "submit": true, "agent_only": true}))
	if err != nil {
		t.Fatal(err)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome == ops.OutcomeDelivered || len(e.host.eventLog()) != 0 {
		t.Fatalf("agent-only into a shell: %+v, events %v", res, e.host.eventLog())
	}
}
