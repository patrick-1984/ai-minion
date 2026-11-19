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
	// Sinks is the effective state of the remote log sinks.
	Sinks []DiagnosticsFact
}

// sinkFacts describes the remote log sinks as the Hub runs them.
func sinkFacts(list []ops.LogSinkStatus) []DiagnosticsFact {
	out := make([]DiagnosticsFact, 0, len(list))
	for _, st := range list {
		out = append(out, DiagnosticsFact{Label: "Log export (" + st.Name + ")", Value: sinkText(st)})
	}
	return out
}

// sinkText is one remote log sink for a person: off (with the reason), or
// the destination and level with what was sent, queued and dropped.
func sinkText(st ops.LogSinkStatus) string {
	var dropped uint64
	for _, n := range st.Dropped {
		dropped += n
	}
	var b strings.Builder
	if st.Enabled {
		b.WriteString(st.Destination + " at " + st.Level + " and above; sent " + strconv.FormatUint(st.Sent, 10) +
			", queued " + strconv.FormatUint(st.Queued, 10) + ", dropped " + strconv.FormatUint(dropped, 10))
	} else {
		b.WriteString("off")
	}
	b.WriteString(" (configuration " + strconv.FormatUint(st.Generation, 10) + ")")
	if st.LastError != "" {
		b.WriteString("; last error: " + st.LastError)
	}
	return b.String()
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
	page.Sinks = sinkFacts(d.Sinks)
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
