package web

// "Last checked" and "Refresh" (A91): every status view shows when its data
// was last checked and offers a Refresh that asks the Worker again.

import (
	"html"
	"html/template"
	"net/http"
	"sync"
	"time"
)

// refreshEvery is the shortest time between two forced refreshes of one Worker.
const refreshEvery = 10 * time.Second

// refreshNow is the clock of the refresh rate limit (a test replaces it).
var refreshNow = time.Now

// refreshLimiter lets one forced refresh per Worker through per refreshEvery.
type refreshLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var workerRefresh = &refreshLimiter{last: map[string]time.Time{}}

// allow reports whether the Worker may be refreshed now, and counts the refresh.
func (l *refreshLimiter) allow(id string) bool {
	now := refreshNow()
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[id]; ok && now.Sub(at) < refreshEvery {
		return false
	}
	for k, at := range l.last { // forget the old entries
		if now.Sub(at) >= refreshEvery {
			delete(l.last, k)
		}
	}
	l.last[id] = now
	return true
}

// forget gives back the cooldown of an attempt that failed.
func (l *refreshLimiter) forget(id string) {
	l.mu.Lock()
	delete(l.last, id)
	l.mu.Unlock()
}

// checkedTime renders a moment as <time>: the page script adds "(N min ago)".
func checkedTime(ms *int64, none string) template.HTML {
	if ms == nil || *ms == 0 {
		return template.HTML(html.EscapeString(none))
	}
	t := time.UnixMilli(*ms).UTC()
	return template.HTML(`<time datetime="` + t.Format(time.RFC3339) + `">` + t.Format("Jan 2, 2006 15:04 UTC") + `</time>`)
}

// writeResultInfo is the confirmation line that htmx swaps into a result area.
func writeResultInfo(w http.ResponseWriter, text string) {
	writeFragment(w, []byte(`<p class="notice notice-info" role="status">`+html.EscapeString(text)+`</p>`))
}
