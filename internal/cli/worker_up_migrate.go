package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// stopForegroundForService releases a foreground Worker's state lock before
// the service tries to take it. An existing service is restarted by its manager.
func stopForegroundForService(ctx context.Context, state string, owner *ownerSetup) error {
	if owner == nil {
		return stopPriorWorker(ctx, state, true, client.Local())
	}
	var out bytes.Buffer
	args := []string{"worker", "down", "--foreground-in", state}
	if err := runAsUser(ctx, owner.Target, owner.Binary, args, &out); err != nil {
		return fmt.Errorf("check the previous Worker of %s: %w: %s", owner.Target.User, err, strings.TrimSpace(out.String()))
	}
	return nil
}

func stopPriorWorker(ctx context.Context, state string, foregroundOnly bool, c client.Backend) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := c.Call(ctx, agent.OpLocalStatus, nil)
	if errors.Is(err, client.ErrWorkerNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	var live agent.LiveStatusReply
	if err := json.Unmarshal(raw, &live); err != nil {
		return err
	}
	if foregroundOnly && live.RunMode != agent.RunModeForeground {
		return nil
	}
	if live.StateDir == "" || !sameDir(live.StateDir, state) {
		return fmt.Errorf("a Worker already serves %q; stop it with minion worker down before starting %q", live.StateDir, state)
	}
	if _, err = c.Call(ctx, agent.OpLocalStop, map[string]string{"instance": live.Instance}); err != nil &&
		!errors.Is(err, client.ErrConnectionLost) && !errors.Is(err, client.ErrWorkerNotRunning) {
		return err
	}
	for !workerStateFree(state) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the previous Worker to stop: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}
