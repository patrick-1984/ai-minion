package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

type statusOnly struct {
	client.Backend
	reply agent.LiveStatusReply
	err   error
}

func (s statusOnly) Call(context.Context, string, any) (json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	return json.Marshal(s.reply)
}

// A foreground Worker with a Hub is not replaced by a background Worker
// whose saved settings name no Hub (it would run alone).
func TestMigrationKeepsTheHub(t *testing.T) {
	dir := t.TempDir()
	hubbed := agent.LiveStatusReply{StateDir: dir, RunMode: agent.RunModeForeground}
	hubbed.State = "connected"
	if err := checkMigrationKeepsHub(context.Background(), dir, false, statusOnly{reply: hubbed}); err == nil ||
		!strings.Contains(err.Error(), "--server") {
		t.Fatalf("err = %v", err)
	}
	if err := checkMigrationKeepsHub(context.Background(), dir, true, statusOnly{reply: hubbed}); err != nil {
		t.Fatalf("with a Hub: %v", err)
	}
	solo := hubbed
	solo.Reason = agent.ReasonNoHubConfigured
	if err := checkMigrationKeepsHub(context.Background(), dir, false, statusOnly{reply: solo}); err != nil {
		t.Fatalf("a Solo Worker: %v", err)
	}
	other := hubbed
	other.StateDir = t.TempDir()
	if err := checkMigrationKeepsHub(context.Background(), dir, false, statusOnly{reply: other}); err != nil {
		t.Fatalf("another profile: %v", err)
	}
	if err := checkMigrationKeepsHub(context.Background(), dir, false, statusOnly{err: client.ErrWorkerNotRunning}); err != nil {
		t.Fatalf("nothing running: %v", err)
	}
}
