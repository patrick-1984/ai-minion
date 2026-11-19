package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Headers of the live monitoring notice (A49 design v2, §5.7). The Hub puts the
// policy in force on every API response and refuses once a content-bearing
// request of a client that last saw a less strict policy.
const (
	headerMonitoring     = "Minion-Monitoring"
	headerMonitoringSeen = "Minion-Monitoring-Seen"
)

// MonitoringSeen remembers, per Hub, the monitoring policy version this
// client last showed to its user: in memory for the life of the process and,
// when the user's config directory can be read and written, in a small private
// JSON file. A file that cannot be used (unreadable, corrupt, a read-only
// mount) is not trusted: the client then asks the Hub for the policy before a
// content call (a notice preflight, see HubBackend.preflight), shows the
// notice and goes on, so it is neither captured before the notice nor refused
// over and over.
type MonitoringSeen struct {
	path string
	mu   sync.Mutex
	mem  map[string]int64
	// writable caches whether the state directory accepts a file: 0 unknown, 1 yes, 2 no.
	writable int
}

// NewMonitoringSeen returns the store in path.
func NewMonitoringSeen(path string) *MonitoringSeen {
	return &MonitoringSeen{path: path, mem: map[string]int64{}}
}

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

// usable reports whether the file can hold state: the directory accepts a file
// and the file, if there is one, parses.
func (m *MonitoringSeen) usable() bool {
	if m.path == "" {
		return false
	}
	if m.writable == 0 {
		m.writable = 2
		if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err == nil {
			if f, err := os.CreateTemp(filepath.Dir(m.path), ".probe-*"); err == nil {
				_ = f.Close()
				_ = os.Remove(f.Name())
				m.writable = 1
			}
		}
	}
	if m.writable != 1 {
		return false
	}
	_, err := m.load()
	return err == nil
}

// Get returns the version last shown for hub (0 when never), and whether the
// answer can be trusted: the file works, or this process already learned one.
func (m *MonitoringSeen) Get(hub string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, haveMem := m.mem[hub]
	if m.usable() {
		all, _ := m.load()
		if v := all[hub]; v > mem {
			mem = v
		}
		return mem, true
	}
	return mem, haveMem
}

// Put records that the user saw version v of hub.
func (m *MonitoringSeen) Put(hub string, v int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v > m.mem[hub] {
		m.mem[hub] = v
	}
	if !m.usable() {
		return
	}
	all, _ := m.load()
	if all[hub] == v {
		return
	}
	all[hub] = v
	b, _ := json.Marshal(all)
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
		b.mon.Put(b.monKey, m.version)
	}
	if m.active() && b.monNotice != nil {
		b.monOnce.Do(func() { b.monNotice(m.notice()) })
	}
}

// preflight asks the Hub for its monitoring policy without any credential
// (every API response carries the header, also the answer to the bare API
// prefix), shows the notice and remembers the version for this process. It is
// the way a client with no usable state is told before its first content call.
func (b *HubBackend) preflight(ctx context.Context) (int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := *b.base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("User-Agent", b.userAgent)
	resp, err := b.hc.Do(req)
	if err != nil {
		return 0, false
	}
	_ = resp.Body.Close()
	m, ok := parseMonitoring(resp.Header)
	if !ok {
		return 0, false
	}
	b.mon.Put(b.monKey, m.version)
	if m.active() && b.monNotice != nil {
		b.monOnce.Do(func() { b.monNotice(m.notice()) })
	}
	return m.version, true
}
