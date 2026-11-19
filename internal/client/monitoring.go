package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Headers of the live monitoring notice (A49 design v2, §5.7). The Hub puts the
// policy in force on every API response and refuses once a content-bearing
// request of a client that last saw a less strict policy.
const (
	headerMonitoring     = "Minion-Monitoring"
	headerMonitoringSeen = "Minion-Monitoring-Seen"
)

// MonitoringSeen remembers, per Hub, the monitoring policy version this
// client last showed to its user. It is a small private JSON file next to the
// client's other state; a file that cannot be read or written means "never
// seen" and the client then does not send the seen header at all (the Hub
// informs it through the response header only), so that it is never refused
// without a way to be let through.
type MonitoringSeen struct {
	path string
	mu   sync.Mutex
}

// NewMonitoringSeen returns the store in path.
func NewMonitoringSeen(path string) *MonitoringSeen { return &MonitoringSeen{path: path} }

// DefaultMonitoringPath is the file of the current user; "" when the user has
// no config directory.
func DefaultMonitoringPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "minion", "hub-monitoring.json")
}

func (m *MonitoringSeen) load() (map[string]int64, error) {
	b, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, fmt.Errorf("monitoring state: file too large")
	}
	out := map[string]int64{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns the version last seen for hub (0 when never), and whether the
// store works.
func (m *MonitoringSeen) Get(hub string) (int64, bool) {
	if m == nil || m.path == "" {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.load()
	if err != nil {
		return 0, false
	}
	return all[hub], true
}

// Put records that the user saw version v of hub.
func (m *MonitoringSeen) Put(hub string, v int64) {
	if m == nil || m.path == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.load()
	if err != nil {
		all = map[string]int64{}
	}
	if all[hub] == v {
		return
	}
	all[hub] = v
	b, _ := json.Marshal(all)
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, m.path)
}

// WithHubMonitoring makes the backend send the version of the monitoring
// policy it last showed (Minion-Monitoring-Seen), remember the one the Hub
// reports after the user was told, and call notice (once per run) while
// sessions are monitored. notice may be nil.
func WithHubMonitoring(s *MonitoringSeen, notice func(string)) HubOption {
	return func(b *HubBackend) { b.mon, b.monNotice = s, notice }
}

// monitoringHeader is a parsed Minion-Monitoring header.
type monitoringHeader struct {
	version int64
	level   string
	input   bool
	traffic bool
}

func parseMonitoring(h http.Header) (monitoringHeader, bool) {
	v := h.Get(headerMonitoring)
	if v == "" {
		return monitoringHeader{}, false
	}
	var out monitoringHeader
	ok := false
	for _, part := range strings.Split(v, ";") {
		k, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch k {
		case "v":
			n, err := strconv.ParseInt(val, 10, 64)
			if err == nil && n >= 0 {
				out.version, ok = n, true
			}
		case "level":
			out.level = val
		case "input":
			out.input = val == "on"
		case "traffic":
			out.traffic = val == "payload"
		}
	}
	return out, ok
}

func (m monitoringHeader) active() bool { return (m.level != "" && m.level != "off") || m.traffic }

func (m monitoringHeader) notice() string {
	var parts []string
	if m.level != "" && m.level != "off" {
		in := "off"
		if m.input {
			in = "on"
		}
		parts = append(parts, m.level+", input "+in)
	}
	if m.traffic {
		parts = append(parts, "Worker traffic with frame contents")
	}
	return "Sessions are monitored: " + strings.Join(parts, "; ") + " (policy v" + strconv.FormatInt(m.version, 10) + ")"
}

// noteMonitoring reads the policy of a response. A policy that records
// nothing needs no notice and is remembered as seen; an active one is shown
// once per run. refused is true for the answer monitoring_changed: the user
// is told by the error, so the version is remembered and the next call goes
// through.
func (b *HubBackend) noteMonitoring(h http.Header, refused bool) {
	if b.mon == nil {
		return
	}
	m, ok := parseMonitoring(h)
	if !ok {
		return
	}
	if !m.active() || refused {
		b.mon.Put(b.label, m.version)
	}
	if m.active() && b.monNotice != nil {
		b.monOnce.Do(func() { b.monNotice(m.notice()) })
	}
}
