package selfupdate

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine is a Docker Engine API on a Unix socket with one Hub container.
type fakeEngine struct {
	mu       sync.Mutex
	calls    []string
	created  map[string]any
	running  map[string]bool
	names    map[string]string // id -> name
	failNew  bool              // the new container never becomes ready
	tagged   string
	removed  []string
	dataDir  string
	oldImage string
}

func (f *fakeEngine) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/"+apiVersion)
	f.calls = append(f.calls, r.Method+" "+p)
	switch {
	case r.Method == http.MethodGet && p == "/containers/json":
		_, _ = w.Write([]byte("[]"))
	case p == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		if _, ok := f.names[id]; !ok {
			http.Error(w, `{"message":"no such container"}`, 404)
			return
		}
		img := f.oldImage
		if id == "new" {
			img = "ghcr.io/patrick-1984/minion@sha256:new"
		}
		c := map[string]any{
			"Id": id, "Name": "/" + f.names[id],
			"Config": map[string]any{"Image": img, "Env": []string{"MINION_DATA_DIR=" + f.dataDir, "MINION_TLS=off"},
				"Labels": map[string]string{composeImageLabel: "sha256:old", "com.docker.compose.service": "hub"}},
			"HostConfig":      map[string]any{"NetworkMode": "minion_default", "Binds": []string{"./hub-data:/data"}},
			"Mounts":          []map[string]any{{"Type": "volume", "Name": "anon123", "Destination": "/extra", "RW": true}},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"minion_default": map[string]any{"Aliases": []string{"hub"}, "IPAddress": "10.0.0.2"}}},
			"State":           map[string]any{"Running": f.running[id]},
		}
		_ = json.NewEncoder(w).Encode(c)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/"):
		_, _ = w.Write([]byte(`{"Id":"sha256:newimageid"}`))
	case p == "/containers/create":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = body
		f.names["new"] = r.URL.Query().Get("name")
		_, _ = w.Write([]byte(`{"Id":"new"}`))
	case strings.HasSuffix(p, "/start"):
		f.running[strings.Split(p, "/")[2]] = true
		w.WriteHeader(204)
	case strings.HasSuffix(p, "/stop"):
		f.running[strings.Split(p, "/")[2]] = false
		w.WriteHeader(204)
	case strings.HasSuffix(p, "/rename"):
		f.names[strings.Split(p, "/")[2]] = r.URL.Query().Get("name")
		w.WriteHeader(204)
	case strings.HasSuffix(p, "/tag"):
		f.tagged = r.URL.Query().Get("repo") + ":" + r.URL.Query().Get("tag")
		w.WriteHeader(201)
	case r.Method == http.MethodDelete:
		id := strings.Split(p, "/")[2]
		f.removed = append(f.removed, id)
		delete(f.names, id)
		w.WriteHeader(204)
	default:
		w.WriteHeader(204)
	}
	_, _ = io.Copy(io.Discard, r.Body)
}

func startEngine(t *testing.T, f *fakeEngine) *Docker {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(f.handler)}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewDocker(sock)
}

func newEngine(t *testing.T) *fakeEngine {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "minion.db"), []byte("v1 data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fakeEngine{created: nil, running: map[string]bool{"old": true}, names: map[string]string{"old": "minion-hub-1"},
		dataDir: data, oldImage: "ghcr.io/patrick-1984/minion:0.9"}
}

func params(f *fakeEngine, ready bool) ReplaceParams {
	return ReplaceParams{OldID: "old", ImageRef: "ghcr.io/patrick-1984/minion@sha256:new",
		ChannelRepo: "ghcr.io/patrick-1984/minion", ChannelTag: "0.9", From: "v0.9.0",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ReadyTimeout: 8 * time.Second, StopTimeout: time.Second,
		Ready: func(context.Context, Container) bool { return ready }}
}

func TestReplaceSwapsTheContainerAndKeepsItsConfiguration(t *testing.T) {
	f := newEngine(t)
	d := startEngine(t, f)
	if err := Replace(context.Background(), d, params(f, true)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names["new"] != "minion-hub-1" || f.created["Image"] != "ghcr.io/patrick-1984/minion@sha256:new" {
		t.Fatalf("created %v as %q", f.created["Image"], f.names["new"])
	}
	labels := f.created["Labels"].(map[string]any)
	if labels[composeImageLabel] != "sha256:newimageid" || labels["com.docker.compose.service"] != "hub" {
		t.Errorf("labels %v", labels)
	}
	host := f.created["HostConfig"].(map[string]any)
	mounts, _ := json.Marshal(host["Mounts"])
	if !strings.Contains(string(mounts), `"Source":"anon123"`) {
		t.Errorf("the anonymous volume was not pinned: %s", mounts)
	}
	if f.tagged != "ghcr.io/patrick-1984/minion:0.9" {
		t.Errorf("tag moved to %q", f.tagged)
	}
	if len(f.removed) != 1 || f.removed[0] != "old" {
		t.Errorf("removed %v", f.removed)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir, backupPrefix+"v0.9.0", "minion.db")); string(b) != "v1 data" {
		t.Error("no backup of the data directory")
	}
}

// A new release that never becomes ready is removed; the data directory is
// restored and the previous Hub runs again under its name.
func TestReplaceRollsBack(t *testing.T) {
	f := newEngine(t)
	d := startEngine(t, f)
	p := params(f, false)
	p.Ready = func(context.Context, Container) bool {
		// The new release migrated the database before failing.
		_ = os.WriteFile(filepath.Join(f.dataDir, "minion.db"), []byte("v2 schema"), 0o600)
		return false
	}
	if err := Replace(context.Background(), d, p); err == nil {
		t.Fatal("no error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running["old"] || f.names["old"] != "minion-hub-1" {
		t.Fatalf("old: running=%v name=%q", f.running["old"], f.names["old"])
	}
	if _, ok := f.names["new"]; ok {
		t.Error("the failed container was not removed")
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir, "minion.db")); string(b) != "v1 data" {
		t.Errorf("data = %q, want the copy from before the update", b)
	}
	if f.tagged != "" {
		t.Error("the tag moved although the update failed")
	}
}

func TestOnlyChannelTagsSelfUpdate(t *testing.T) {
	for _, tc := range []struct {
		image string
		ok    bool
	}{
		{"ghcr.io/patrick-1984/minion:0.9", true},
		{"ghcr.io/patrick-1984/minion:0", true},
		{"ghcr.io/patrick-1984/minion:latest", true},
		{"ghcr.io/patrick-1984/minion", true},
		{"ghcr.io/patrick-1984/minion:v0.9.0", false},
		{"ghcr.io/patrick-1984/minion@sha256:abc", false},
		{"example.com/other:0.9", false},
	} {
		f := newEngine(t)
		f.oldImage = tc.image
		d := startEngine(t, f)
		sock := filepath.Join(t.TempDir(), "present")
		_ = os.WriteFile(sock, nil, 0o600)
		// findSelf looks the container up by hostname: name the fake one so.
		host, _ := os.Hostname()
		f.names[host] = "minion-hub-1"
		delete(f.names, "old")
		f.running[host] = true
		_, why := Detect(context.Background(), d, sock)
		if (why == "") != tc.ok {
			t.Errorf("%s: reason %q", tc.image, why)
		}
	}
}

func TestLockExcludesASecondUpdateAndStaleLocksAreTaken(t *testing.T) {
	f := newEngine(t)
	d := startEngine(t, f)
	if err := acquireLock(context.Background(), d, f.dataDir); err != nil {
		t.Fatal(err)
	}
	// No helper container exists: the lock is stale and taken again.
	if err := acquireLock(context.Background(), d, f.dataDir); err != nil {
		t.Fatalf("stale lock: %v", err)
	}
}
