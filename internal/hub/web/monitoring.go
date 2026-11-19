package web

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
)

// MonitoringView is the monitoring policy a page was rendered with (A49 T16a).
// Version is embedded in the page, so that a form post or a fragment fetch of
// a page that is older than the policy can be refused once with the notice
// (the Hub compares it with the policy in force). Notice is the banner text,
// empty while nothing is monitored.
type MonitoringView struct {
	Version int64
	Notice  string
}

// monitoringSource is the optional source of the policy in force.
var monitoringSource atomic.Pointer[func() MonitoringView]

// SetMonitoring tells the pages where to read the monitoring policy (the
// Hub's web module passes the compliance Monitor). nil removes the source; the
// returned function removes this source again while it is still the current
// one.
func SetMonitoring(f func() MonitoringView) (release func()) {
	if f == nil {
		monitoringSource.Store(nil)
		return func() {}
	}
	p := &f
	monitoringSource.Store(p)
	return func() { monitoringSource.CompareAndSwap(p, nil) }
}

func currentMonitoring() MonitoringView {
	if p := monitoringSource.Load(); p != nil {
		return (*p)()
	}
	return MonitoringView{}
}

// versionText is the version as the page attribute holds it.
func (m MonitoringView) VersionText() string { return strconv.FormatInt(m.Version, 10) }

// seenHeader is the request header that carries the policy version a page
// was rendered with, to the Hub's API through [HandlerCaller].
const seenHeader = "Minion-Monitoring-Seen"

// copySeen forwards the version a browser page sent.
func copySeen(dst, src http.Header) {
	if v := src.Get(seenHeader); v != "" {
		dst.Set(seenHeader, v)
	}
}

// monitorSignal carries, from the in-process API call to the browser's
// response, the monitoring policy of a refusal (409 monitoring_changed). The
// page script takes the new version from this response header, never from the
// text of a response (session content can look like anything).
type monitorSignal struct {
	mu sync.Mutex
	v  string
}

type monitorSignalKey struct{}

func (m *monitorSignal) set(v string) {
	m.mu.Lock()
	m.v = v
	m.mu.Unlock()
}

func (m *monitorSignal) get() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.v
}

// monitorWriter adds the refusal's policy header when the response starts.
type monitorWriter struct {
	http.ResponseWriter
	sig  *monitorSignal
	done bool
}

func (w *monitorWriter) stamp() {
	if w.done {
		return
	}
	w.done = true
	if v := w.sig.get(); v != "" {
		w.Header().Set("Minion-Monitoring", v)
	}
}

func (w *monitorWriter) WriteHeader(code int) { w.stamp(); w.ResponseWriter.WriteHeader(code) }

func (w *monitorWriter) Write(b []byte) (int, error) { w.stamp(); return w.ResponseWriter.Write(b) }

// Unwrap lets http.ResponseController reach the real writer (flush, deadlines).
func (w *monitorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *monitorWriter) Flush() {
	w.stamp()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// noteRefusal records the policy header of a refused API answer for the
// browser's response.
func noteRefusal(r *http.Request, h http.Header) {
	if sig, ok := r.Context().Value(monitorSignalKey{}).(*monitorSignal); ok {
		sig.set(h.Get("Minion-Monitoring"))
	}
}
