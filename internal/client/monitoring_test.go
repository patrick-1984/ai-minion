package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestMonitoringSeenFileRoundTripAndBadState(t *testing.T) {
	dir := t.TempDir()
	m := NewMonitoringSeen(filepath.Join(dir, "sub", "m.json"))
	if v, ok := m.Get("https://hub"); v != 0 || !ok {
		t.Fatalf("empty = %d %v", v, ok)
	}
	m.Put("https://hub", 4)
	m.Put("https://other", 9)
	if v, _ := m.Get("https://hub"); v != 4 {
		t.Fatalf("v = %d", v)
	}
	fi, err := os.Stat(m.path)
	if err != nil || fi.Mode().Perm()&0o077 != 0 && !strings.Contains(os.Getenv("OS"), "Windows") {
		t.Fatalf("mode %v %v", fi, err)
	}
	// A corrupt file: no version, not working, so no seen header is sent.
	_ = os.WriteFile(m.path, []byte("{broken"), 0o600)
	if _, ok := m.Get("https://never-seen"); ok {
		t.Fatal("a corrupt state file must read as not working")
	}
	if v, ok := m.Get("https://hub"); v != 4 || !ok {
		t.Fatalf("what this process learned is kept in memory: %d %v", v, ok)
	}
	if _, ok := (*MonitoringSeen)(nil).Get("x"); ok {
		t.Fatal("nil store")
	}
}

// A Hub that refuses once: the client prints the notice as the error, stores
// the version it was told, and the next call goes through with that version.
func TestClientSeesRefusalOnceThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	var seenHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seenHeaders = append(seenHeaders, r.Header.Get(headerMonitoringSeen))
		w.Header().Set(headerMonitoring, "v=3; level=output; input=on")
		if r.Header.Get(headerMonitoringSeen) != "3" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"monitoring_changed","message":"Sessions are monitored: output, input on (policy v3). Check it and repeat the request; nothing was sent."}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"delivered"}`))
	}))
	defer srv.Close()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("mn_"+strings.Repeat("a", 52)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var notices []string
	b, err := NewHub(HubConfig{URL: srv.URL, APIKeyFile: keyFile},
		WithHubMonitoring(NewMonitoringSeen(filepath.Join(t.TempDir(), "m.json")), func(s string) { notices = append(notices, s) }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Call(t.Context(), "send_text", map[string]any{"request_id": "x"})
	var pe *proto.Error
	if err == nil || !errors.As(err, &pe) || pe.Code != proto.CodeMonitoringChanged || !strings.Contains(pe.Message, "policy v3") {
		t.Fatalf("first call: %v", err)
	}
	if _, err := b.Call(t.Context(), "send_text", map[string]any{"request_id": "x"}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if strings.Join(seenHeaders, ",") != "0,3" {
		t.Fatalf("seen headers = %v", seenHeaders)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "output, input on") {
		t.Fatalf("notices = %v", notices)
	}
}

func TestParseMonitoringHeader(t *testing.T) {
	h := http.Header{}
	h.Set(headerMonitoring, "v=12; level=actions; input=off; traffic=payload")
	m, ok := parseMonitoring(h)
	if !ok || m.version != 12 || m.level != "actions" || m.input || !m.traffic || !m.active() {
		t.Fatalf("%+v", m)
	}
	if !strings.Contains(m.notice(), "frame contents") {
		t.Fatal(m.notice())
	}
	if _, ok := parseMonitoring(http.Header{}); ok {
		t.Fatal("no header")
	}
	h.Set(headerMonitoring, "v=1; level=off; input=off")
	if m, _ := parseMonitoring(h); m.active() {
		t.Fatal("off is not active")
	}
}

func writeKey(t *testing.T) string {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("mn_"+strings.Repeat("a", 52)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyFile
}

// monitoredHub is a Hub that answers the bare API prefix with its policy header
// (as the real one does) and refuses a content call that does not carry the
// current version.
func monitoredHub(t *testing.T, version string, calls *atomic.Int32, preflights *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerMonitoring, "v="+version+"; level=output; input=on")
		if strings.HasSuffix(r.URL.Path, "/api/v1/") {
			preflights.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		if r.Header.Get(headerMonitoringSeen) != version {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"monitoring_changed","message":"Sessions are monitored (policy v` + version + `)"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"delivered"}`))
	}))
}

// Two Hubs behind one origin (different base paths) never share an
// acknowledgment.
func TestMonitoringAcknowledgmentIsPerHubIdentityIncludingBasePath(t *testing.T) {
	var calls, pre atomic.Int32
	srv := monitoredHub(t, "1", &calls, &pre)
	defer srv.Close()
	store := NewMonitoringSeen(filepath.Join(t.TempDir(), "m.json"))
	key := writeKey(t)
	a, err := NewHub(HubConfig{URL: srv.URL + "/a", APIKeyFile: key}, WithHubMonitoring(store, nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewHub(HubConfig{URL: srv.URL + "/b", APIKeyFile: key}, WithHubMonitoring(store, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Call(t.Context(), "send_text", map[string]any{"request_id": "x"}); err == nil {
		t.Fatal("Hub A: the first content call must be refused")
	}
	if _, err := a.Call(t.Context(), "send_text", map[string]any{"request_id": "x"}); err != nil {
		t.Fatalf("Hub A second call: %v", err)
	}
	if _, err := b.Call(t.Context(), "send_text", map[string]any{"request_id": "x"}); err == nil {
		t.Fatal("Hub B inherited the acknowledgment of Hub A")
	}
}

// A state file that cannot be used (corrupt, or a directory that takes no file)
// does not let a content call through before the notice, and does not refuse
// it for ever either: the client asks the Hub first, shows the notice, and
// sends the current version.
func TestUnusableStateUsesANoticePreflight(t *testing.T) {
	for _, name := range []string{"corrupt", "unwritable"} {
		t.Run(name, func(t *testing.T) {
			var calls, pre atomic.Int32
			srv := monitoredHub(t, "4", &calls, &pre)
			defer srv.Close()
			dir := t.TempDir()
			path := filepath.Join(dir, "m.json")
			switch name {
			case "corrupt":
				_ = os.WriteFile(path, []byte("{broken"), 0o600)
			case "unwritable":
				// A file where the directory should be: nothing can be created under it.
				blocker := filepath.Join(dir, "blocker")
				_ = os.WriteFile(blocker, []byte("x"), 0o600)
				path = filepath.Join(blocker, "m.json")
			}
			var notices []string
			b, err := NewHub(HubConfig{URL: srv.URL, APIKeyFile: writeKey(t)},
				WithHubMonitoring(NewMonitoringSeen(path), func(s string) { notices = append(notices, s) }))
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := b.Call(t.Context(), "send_text", map[string]any{"request_id": "x"}); err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
			}
			if pre.Load() != 1 || len(notices) != 1 || !strings.Contains(notices[0], "policy v4") {
				t.Fatalf("preflights %d notices %v", pre.Load(), notices)
			}
		})
	}
}
