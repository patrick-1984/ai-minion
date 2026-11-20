//go:build windows

package cli

import (
	"context"
	"errors"
	"io"

	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/service"
)

// runAsUser is not used on Windows: "worker up" never runs for another user there.
func runAsUser(context.Context, service.Target, string, []string, io.Writer) error {
	return errors.New("running a command as another user is not supported on Windows")
}

func runOwnerHooks(context.Context, *ownerSetup, agent.Settings, io.Reader, io.Writer) error {
	return errors.New("a Windows Worker runs as the current user")
}
