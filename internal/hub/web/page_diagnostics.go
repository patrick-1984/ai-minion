package web

// Administration page "Diagnostics": how the Hub runs and how it sees the
// browser's own request (peer, client address, forwarding headers, effective
// scheme), for troubleshooting a reverse proxy or the public URL. It reads
// everything through hub_diagnostics; the in-process caller forwards the
// browser's address and forwarding headers, so the request facts are those of
// the browser's request.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const pathAdminDiagnostics = "/admin/diagnostics"

// DiagnosticsPage is what diagnostics.html sees as .Data.
type DiagnosticsPage struct {
	Hub     []DiagnosticsFact
	Request []DiagnosticsFact
}

// DiagnosticsFact is one label and value of the grid.
type DiagnosticsFact struct {
	Label, Value string
}

func diagnosticsData(r *http.Request, c OpCaller) (any, error) {
	var d ops.HubDiagnosticsResult
	if err := c.CallOp(r, ops.OpHubDiagnostics, ops.HubDiagnosticsArgs{}, &d); err != nil {
		return nil, err
	}
	started := d.StartedAt
	page := DiagnosticsPage{
		Hub: []DiagnosticsFact{
			{"Version", d.Version},
			{"Commit", d.Commit},
			{"Started", formatTime(&started, "")},
			{"Uptime", (time.Duration(d.UptimeSeconds) * time.Second).String()},
			{"Public URL", orText(d.PublicURL, "not set (MINION_PUBLIC_URL)")},
			{"Hub terminates TLS", yesNo(d.TLS)},
			{"Trusted proxies", orText(strings.Join(d.TrustedProxies, ", "), "none (MINION_TRUSTED_PROXIES)")},
			{"Workers connected", strconv.Itoa(d.WorkersConnected)},
			{"Workers waiting for approval", strconv.Itoa(d.WorkersPending)},
		},
	}
	q := d.Request
	var headers []string
	for _, h := range q.ForwardedHeaders {
		headers = append(headers, h.Name+": "+h.Value)
	}
	honoured := "no: the forwarding headers are ignored"
	if q.PeerTrustedProxy {
		honoured = "yes: the forwarding headers are honoured"
	}
	page.Request = []DiagnosticsFact{
		{"Peer address", orText(q.PeerAddress, "unknown")},
		{"Peer is a trusted proxy", honoured},
		{"Client IP", orText(q.ClientIP, "unknown")},
		{"Forwarding headers received", orText(strings.Join(headers, "; "), "none")},
		{"Effective scheme", q.Scheme},
		{"Effective HTTPS", yesNo(q.EffectiveHTTPS)},
	}
	return page, nil
}

func orText(v, empty string) string {
	if v == "" {
		return empty
	}
	return v
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func init() {
	RegisterPage(Page{Pattern: pathAdminDiagnostics, Title: "Diagnostics", Template: "diagnostics.html", Data: adminPageData(diagnosticsData)})
	RegisterNav(NavItem{Label: "Diagnostics", Path: pathAdminDiagnostics, Order: adminNavOrder + 28, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-panel"})
}
