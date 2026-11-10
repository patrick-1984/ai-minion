package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Health answers GET /healthz and GET /readyz (AD-18).
//
// /healthz is liveness: 200 as long as the process serves requests.
//
// /readyz is readiness: 200 only when the Hub finished starting (every
// module initialised and every migration applied, because the store applies
// them when it opens) and every registered check passes, for example that the
// database still answers. Before startup completes and while the Hub shuts
// down it answers 503. The response names the failing check, never its error
// text, so that nothing internal leaks to an unauthenticated caller.
//
// Modules get Health as a service and add their checks with
// [Health.AddReadyCheck].
type Health struct {
	started atomic.Bool
	closing atomic.Bool
	timeout time.Duration

	mu     sync.Mutex
	checks []readyCheck
}

type readyCheck struct {
	name string
	fn   func(ctx context.Context) error
}

// checkTimeout bounds one readiness probe.
const checkTimeout = 2 * time.Second

// NewHealth returns a Health that is live but not yet ready.
func NewHealth() *Health { return &Health{timeout: checkTimeout} }

// AddReadyCheck registers a readiness check. fn must be quick; it runs on
// every /readyz request under a short deadline.
func (h *Health) AddReadyCheck(name string, fn func(ctx context.Context) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks = append(h.checks, readyCheck{name: name, fn: fn})
}

// MarkStarted records that startup is complete; /readyz can now succeed.
func (h *Health) MarkStarted() { h.started.Store(true) }

// Started reports whether [Health.MarkStarted] was called.
func (h *Health) Started() bool { return h.started.Load() }

// MarkStopping makes /readyz fail from now on, so that a load balancer stops
// sending traffic while the Hub drains.
func (h *Health) MarkStopping() { h.closing.Store(true) }

// Ready reports whether the Hub is ready. When it is not, failing names the
// reason: "starting", "shutting_down" or the name of the failing check.
func (h *Health) Ready(ctx context.Context) (failing string, ok bool) {
	switch {
	case h.closing.Load():
		return "shutting_down", false
	case !h.started.Load():
		return "starting", false
	}
	h.mu.Lock()
	checks := append([]readyCheck(nil), h.checks...)
	h.mu.Unlock()
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].name < checks[j].name })
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, h.timeout)
		err := runCheck(cctx, c.fn)
		cancel()
		if err != nil {
			return c.name, false
		}
	}
	return "", true
}

func runCheck(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = context.Canceled
		}
	}()
	return fn(ctx)
}

// Register adds /healthz and /readyz to mux.
func (h *Health) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.serveHealthz)
	mux.HandleFunc("HEAD /healthz", h.serveHealthz)
	mux.HandleFunc("GET /readyz", h.serveReadyz)
	mux.HandleFunc("HEAD /readyz", h.serveReadyz)
}

func (h *Health) serveHealthz(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Health) serveReadyz(w http.ResponseWriter, r *http.Request) {
	if failing, ok := h.Ready(r.Context()); !ok {
		writeStatus(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": failing})
		return
	}
	writeStatus(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeStatus(w http.ResponseWriter, code int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
