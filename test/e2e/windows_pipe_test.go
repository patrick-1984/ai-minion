//go:build e2e && windows

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// TestWindowsPipeSessionOps (Story 8.2) drives a foreground Worker that is
// already running (windows.yml, job worker-foreground) over its named pipe with
// the real client: sessions_list, session_create (a ConPTY session), send_text
// into that session and sessions_list again, which must show the new session.
// These are the operations behind `mn sessions_list`, `mn session_create` and
// `mn send_text`; the `mn` CLI (Story 2.9) is not part of this test.
//
// It is skipped unless MINION_E2E_PIPE=1, because it needs a running Worker.
func TestWindowsPipeSessionOps(t *testing.T) {
	if os.Getenv("MINION_E2E_PIPE") != "1" {
		t.Skip("set MINION_E2E_PIPE=1 with a Worker running in the foreground")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := client.Local()

	list := func() []ops.SessionInfo {
		t.Helper()
		raw, err := c.Call(ctx, ops.OpSessionsList, ops.SessionsListArgs{})
		if err != nil {
			t.Fatalf("sessions_list: %v", err)
		}
		var res ops.SessionsListResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("sessions_list result %s: %v", raw, err)
		}
		return res.Sessions
	}
	has := func(ss []ops.SessionInfo, name string) bool {
		for _, s := range ss {
			if s.Name == name {
				return true
			}
		}
		return false
	}

	const name = "e2e-pipe"
	if has(list(), name) {
		t.Fatalf("session %s exists before the test", name)
	}

	raw, err := c.Call(ctx, ops.OpSessionCreate, ops.SessionCreateArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		Name:      name,
		Argv:      []string{"cmd.exe"},
	})
	if err != nil {
		t.Fatalf("session_create: %v", err)
	}
	var created ops.WriteResult
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("session_create result %s: %v", raw, err)
	}
	if created.Outcome != ops.OutcomeDelivered {
		t.Fatalf("session_create outcome = %q (%s), want delivered", created.Outcome, raw)
	}
	// There is no op that ends a session in release 1: the workflow stops the
	// Worker, which ends its ConPTY sessions.

	// A session of the shell kind needs force for send_text (a guard of the
	// op), which the caller states in the same call.
	const marker = "minion-e2e-marker-8-2"
	raw, err = c.Call(ctx, ops.OpSendText, ops.SendTextArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: true},
		Target:    ops.Target{SessionName: name},
		Text:      marker,
	})
	if err != nil {
		t.Fatalf("send_text: %v", err)
	}
	var sent ops.WriteResult
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("send_text result %s: %v", raw, err)
	}
	if sent.Outcome != ops.OutcomeDelivered {
		t.Fatalf("send_text outcome = %q (%s), want delivered", sent.Outcome, raw)
	}

	// The text reached the pseudo console: cmd.exe echoes it on its prompt line.
	deadline := time.Now().Add(20 * time.Second)
	var screen string
	for {
		raw, err = c.Call(ctx, ops.OpSessionSnapshot, ops.SessionSnapshotArgs{Target: ops.Target{SessionName: name}})
		if err != nil {
			t.Fatalf("session_snapshot: %v", err)
		}
		var snap ops.SessionSnapshotResult
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatalf("session_snapshot result %s: %v", raw, err)
		}
		screen = strings.Join(snap.Screen.Lines, "\n")
		if strings.Contains(screen, marker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sent text never appeared in the session; screen:\n%s", screen)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if !has(list(), name) {
		t.Fatalf("sessions_list does not show the new session %s", name)
	}
}
