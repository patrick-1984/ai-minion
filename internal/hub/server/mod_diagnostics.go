package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// requestLogModuleOrder runs right after the store, before any module that
// wraps the root handler (the REST framework, 40), so that its wrapper is the
// outermost one and sees every request.
const requestLogModuleOrder = OrderStore + 1

// diagnosticsModuleOrder places the module after the workers module (30) and
// the REST framework (40), whose handler registry it fills.
const diagnosticsModuleOrder = 48

func init() {
	registerRequestLogModule(DefaultRegistry)
	registerDiagnosticsModule(DefaultRegistry, ops.Default)
}

// registerRequestLogModule registers the module that writes one debug line
// per request with the scheme decision (see [requestDebugLog]).
func registerRequestLogModule(r *Registry) {
	err := r.Register("request_log", requestLogModuleOrder, func(_ context.Context, env *Env) error {
		cfg, err := Require[*Config](env)
		if err != nil {
			return err
		}
		nw, err := Require[*Network](env)
		if err != nil {
			return err
		}
		root, err := Require[*Root](env)
		if err != nil {
			return err
		}
		return root.Wrap(requestDebugLog(nw, cfg.logger()))
	})
	if err != nil {
		panic(err)
	}
}

// registerDiagnosticsModule registers the module that binds hub_diagnostics.
// A catalog without the operation (a test catalog) registers nothing.
func registerDiagnosticsModule(r *Registry, catalog *ops.Registry) {
	err := r.Register("diagnostics", diagnosticsModuleOrder, func(_ context.Context, env *Env) error {
		if !catalogHas(catalog, ops.OpHubDiagnostics) {
			return nil
		}
		cfg, err := Require[*Config](env)
		if err != nil {
			return err
		}
		nw, err := Require[*Network](env)
		if err != nil {
			return err
		}
		handlers, err := Require[*api.Registry](env)
		if err != nil {
			return err
		}
		svc, err := Require[*workers.Service](env)
		if err != nil {
			return err
		}
		proxies := make([]string, 0, len(cfg.TrustedProxies))
		for _, p := range cfg.TrustedProxies {
			proxies = append(proxies, p.String())
		}
		src := api.HubDiagnosticsSource{
			Version: version.Version, Commit: version.Commit, StartedAt: time.Now(),
			PublicURL: nw.PublicURL(), TLS: !cfg.TLSOff, TrustedProxies: proxies,
			Request: nw.Diagnose,
			Workers: func() (connected, pending int) { return countWorkers(svc.Registry().List()) },
		}
		if err := api.RegisterHubDiagnostics(handlers, src); err != nil {
			return fmt.Errorf("diagnostics: %w", err)
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
}

// countWorkers counts the connected Workers and those that wait for approval.
func countWorkers(list []workers.Online) (connected, pending int) {
	for _, w := range list {
		if w.Pending {
			pending++
		}
	}
	return len(list), pending
}

// forwardingHeaders are the request headers that the trusted-proxy rules read
// (or that a proxy commonly sets), as hub_diagnostics reports them.
var forwardingHeaders = []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"}

// Diagnose describes how the Hub sees r: the direct peer and whether it is a
// trusted proxy, the client address it resolves, the forwarding headers that
// arrived (honoured or not) and the effective scheme.
func (n *Network) Diagnose(r *http.Request) ops.HubDiagnosticsRequest {
	out := ops.HubDiagnosticsRequest{
		Scheme: n.Scheme(r), EffectiveHTTPS: n.EffectiveHTTPS(r),
		ForwardedHeaders: []ops.HubDiagnosticsHeader{},
	}
	if p := peer(r); p.IsValid() {
		out.PeerAddress, out.PeerTrustedProxy = p.String(), n.trusted(p)
	}
	if ip := n.ClientIP(r); ip.IsValid() {
		out.ClientIP = ip.String()
	}
	for _, h := range forwardingHeaders {
		if v := r.Header.Values(h); len(v) > 0 {
			out.ForwardedHeaders = append(out.ForwardedHeaders, ops.HubDiagnosticsHeader{Name: h, Value: strings.Join(v, ", ")})
		}
	}
	return out
}

// requestDebugLog writes, at debug level only, one line per request with the
// scheme decision: peer, trusted-proxy match, resolved client address, the
// names of the forwarding headers seen, the forwarded protocol, the effective
// scheme and effective HTTPS. The path is logged without its query, which can
// carry codes (an OIDC callback).
func requestDebugLog(nw *Network, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if log.Enabled(r.Context(), slog.LevelDebug) {
				d := nw.Diagnose(r)
				names := make([]string, 0, len(d.ForwardedHeaders))
				for _, h := range d.ForwardedHeaders {
					names = append(names, h.Name)
				}
				log.Debug("http request", "method", r.Method, "path", r.URL.Path,
					"peer", d.PeerAddress, "peer_trusted_proxy", d.PeerTrustedProxy, "client_ip", d.ClientIP,
					"forwarded_headers", strings.Join(names, ","), "forwarded_proto", r.Header.Get("X-Forwarded-Proto"),
					"scheme", d.Scheme, "effective_https", d.EffectiveHTTPS)
			}
			next.ServeHTTP(w, r)
		})
	}
}
