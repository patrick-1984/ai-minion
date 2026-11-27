package cli

// minion worker reconnect: ask the running Worker to reconnect to its Hub now
// instead of waiting for its backoff or its keepalive. The Android app runs it
// when the device's network comes back (or changes).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

func init() {
	Register(func(root *cobra.Command) {
		w := workerCommand(root)
		w.AddCommand(newWorkerReconnectCmd(func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		}))
	})
}

func newWorkerReconnectCmd(call func(ctx context.Context, op string, args any) (json.RawMessage, error)) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "reconnect",
		Short: "Reconnect to the Hub now instead of waiting",
		Long: "Ask the running Worker to reconnect to its Hub now. A Worker that waits before its next attempt dials at\n" +
			"once; a connected Worker checks its link with a ping and redials at once when the Hub does not answer\n" +
			"(a working connection is kept). Use it after the network came back. It does not resume a paused Worker.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			raw, err := call(cctx, agent.OpLocalReconnect, map[string]string{"reason": reason})
			var pe *proto.Error
			switch {
			case errors.Is(err, client.ErrWorkerNotRunning):
				return errors.New("no Worker is running for this user; start it with: minion worker start")
			case errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp:
				return errors.New("the running Worker cannot reconnect on request (it is older than this command, or it has no Hub); restart it to reconnect")
			case err != nil:
				return fmt.Errorf("the Worker did not confirm: %w", err)
			}
			var reply struct {
				Result string `json:"result"`
			}
			_ = json.Unmarshal(raw, &reply)
			out := cmd.OutOrStdout()
			switch reply.Result {
			case agent.ReconnectProbing:
				return writef(out, "The Worker is connected; it checks the link now and reconnects at once if the Hub does not answer.\n")
			case agent.ReconnectIdle:
				return writef(out, "The Worker has nothing to connect to now (paused, not enrolled or no Hub configured).\n")
			}
			return writef(out, "The Worker reconnects to its Hub now.\n")
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "a short reason for the Worker's log (for example network-available)")
	return cmd
}
