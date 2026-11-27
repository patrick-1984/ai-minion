package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func TestWorkerReconnectAsksTheRunningWorker(t *testing.T) {
	for result, want := range map[string]string{
		agent.ReconnectWoken:   "reconnects to its Hub now",
		agent.ReconnectProbing: "checks the link now",
		agent.ReconnectIdle:    "nothing to connect to",
	} {
		var gotOp string
		var gotArgs any
		cmd := newWorkerReconnectCmd(func(_ context.Context, op string, args any) (json.RawMessage, error) {
			gotOp, gotArgs = op, args
			return json.RawMessage(`{"result":"` + result + `"}`), nil
		})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--reason", "network-available"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if gotOp != agent.OpLocalReconnect || gotArgs.(map[string]string)["reason"] != "network-available" || !strings.Contains(out.String(), want) {
			t.Errorf("%s: op %q args %v out %q", result, gotOp, gotArgs, out.String())
		}
	}
}

func TestWorkerReconnectRefusals(t *testing.T) {
	for err, want := range map[error]string{
		client.ErrWorkerNotRunning:                     "no Worker is running",
		proto.NewError(proto.CodeUnsupportedOp, "old"): "cannot reconnect on request",
	} {
		cmd := newWorkerReconnectCmd(func(context.Context, string, any) (json.RawMessage, error) { return nil, err })
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(nil)
		if got := cmd.Execute(); got == nil || !strings.Contains(got.Error(), want) {
			t.Errorf("%v: %v", err, got)
		}
	}
}
