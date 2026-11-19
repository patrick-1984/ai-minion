package web

import (
	"net/http"
	"strconv"
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
