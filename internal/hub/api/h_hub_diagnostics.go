package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// HubDiagnosticsSource is what hub_diagnostics reports; the Hub's diagnostics
// module fills it from its configuration, its network rules and the Worker
// registry.
type HubDiagnosticsSource struct {
	Version, Commit string
	StartedAt       time.Time
	PublicURL       string
	TLS             bool
	TrustedProxies  []string
	// Request describes how the Hub sees r.
	Request func(r *http.Request) ops.HubDiagnosticsRequest
	// Workers counts the connected Workers and those of them that wait for
	// approval.
	Workers func() (connected, pending int)
	// Now defaults to time.Now.
	Now func() time.Time
}

// RegisterHubDiagnostics registers hub_diagnostics on r. It needs manage,
// which only admins hold, and like the other Hub-wide admin operations an API
// key whose scope does not reach the whole Hub is refused.
func RegisterHubDiagnostics(r *Registry, src HubDiagnosticsSource) error {
	if src.Request == nil || src.Workers == nil {
		return errors.New("api: hub_diagnostics needs the request and Worker sources")
	}
	if src.Now == nil {
		src.Now = time.Now
	}
	return HandleTyped(r, ops.OpHubDiagnostics, func(_ context.Context, c *Call, _ ops.HubDiagnosticsArgs) (ops.HubDiagnosticsResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.HubDiagnosticsResult{}, err
		}
		res := ops.HubDiagnosticsResult{
			Version: src.Version, Commit: src.Commit,
			StartedAt:     ops.Timestamp(src.StartedAt.UTC()),
			UptimeSeconds: int64(src.Now().Sub(src.StartedAt) / time.Second),
			PublicURL:     src.PublicURL, TLS: src.TLS,
			TrustedProxies: append([]string{}, src.TrustedProxies...),
			Request:        src.Request(c.Request),
		}
		if res.Request.ForwardedHeaders == nil {
			res.Request.ForwardedHeaders = []ops.HubDiagnosticsHeader{}
		}
		res.WorkersConnected, res.WorkersPending = src.Workers()
		return res, nil
	})
}
