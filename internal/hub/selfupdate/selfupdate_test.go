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
	declared bool              // the old container declares an anonymous volume mount
	label    string            // the channel label of the old container
	noMount  bool              // the data directory is in the container's own layer
	tmpfs    bool              // a tmpfs covers the data directory below its bind mount
	tagged   string
	removed  []string
	dataDir  string
	oldImage string
}

func (f *fakeEngine) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if apiVersion != "" {
		p = strings.TrimPrefix(p, "/"+apiVersion)
	}
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
		mounts := []map[string]any{{"Type": "volume", "Name": "anon123", "Destination": "/extra", "RW": true}}
		if !f.noMount {
			mounts = append(mounts, map[string]any{"Type": "bind", "Source": "/srv/hub-data", "Destination": f.dataDir, "RW": true})
		}
		labels := map[string]string{composeImageLabel: "sha256:old", "com.docker.compose.service": "hub"}
		if f.label != "" {
			labels[ChannelLabel] = f.label
		}
		c := map[string]any{
			"Id": id, "Name": "/" + f.names[id],
			"Config": map[string]any{"Image": img, "Env": []string{"MINION_DATA_DIR=" + f.dataDir, "MINION_TLS=off"},
				"Labels": labels},
			"HostConfig":      hostConfig(f.declared, f.tmpfs, f.dataDir),
			"Mounts":          mounts,
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

func hostConfig(declared, tmpfs bool, dataDir string) map[string]any {
	h := map[string]any{"NetworkMode": "minion_default", "Binds": []string{"./hub-data:/data"}}
	if tmpfs {
		h["Tmpfs"] = map[string]string{dataDir + "/hub": ""}
	}
	if declared {
		h["Mounts"] = []map[string]any{{"Type": "volume", "Target": "/extra"}}
	}
	return h
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
	if labels[ChannelLabel] != "0.9" {
		t.Errorf("the new container does not name its channel tag: %v", labels)
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

// A declared anonymous volume mount (Source "") is pinned to the volume it
// resolved to, or the new Hub would get an empty one.
func TestReplacePinsDeclaredAnonymousMounts(t *testing.T) {
	f := newEngine(t)
	f.declared = true
	d := startEngine(t, f)
	if err := Replace(context.Background(), d, params(f, true)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	mounts, _ := json.Marshal(f.created["HostConfig"].(map[string]any)["Mounts"])
	if !strings.Contains(string(mounts), `"Source":"anon123"`) || strings.Count(string(mounts), `"/extra"`) != 1 {
		t.Errorf("mounts %s", mounts)
	}
}

// A failed copy of the data directory restores nothing: the data stays as
// it is and the previous Hub starts again; no new container is created.
func TestReplaceBackupFailureRestartsTheOldHubWithoutRestoring(t *testing.T) {
	f := newEngine(t)
	d := startEngine(t, f)
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	// An unreadable file makes the copy fail.
	if err := os.WriteFile(filepath.Join(f.dataDir, "secret"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := Replace(context.Background(), d, params(f, true)); err == nil {
		t.Fatal("no error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running["old"] || f.names["old"] != "minion-hub-1" || f.created != nil {
		t.Fatalf("old running=%v name=%q created=%v", f.running["old"], f.names["old"], f.created != nil)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir, "minion.db")); string(b) != "v1 data" {
		t.Errorf("data = %q", b)
	}
}

// The container must run the tag of the configured channel (as Compose made
// it) or a digest of the repository (after an earlier self-update).
func TestOnlyTheConfiguredChannelTagSelfUpdates(t *testing.T) {
	for _, tc := range []struct {
		image, channel, label string
		ok                    bool
	}{
		{"ghcr.io/patrick-1984/minion:0.9", "0.9", "", true},
		{"ghcr.io/patrick-1984/minion:0", "0", "", true},
		{"ghcr.io/patrick-1984/minion:latest", "latest", "", true},
		{"ghcr.io/patrick-1984/minion", "latest", "", true},
		{"ghcr.io/patrick-1984/minion@sha256:abc", "0.9", "0.9", true},
		// after a self-update on 0.9 the channel changed: Compose still says 0.9
		{"ghcr.io/patrick-1984/minion@sha256:abc", "latest", "0.9", false},
		{"ghcr.io/patrick-1984/minion@sha256:abc", "0.9", "", false},
		{"ghcr.io/patrick-1984/minion:0.9", "latest", "", false},
		{"ghcr.io/patrick-1984/minion:latest", "0.9", "", false},
		{"ghcr.io/patrick-1984/minion:v0.9.0", "0.9", "", false},
		{"example.com/other:0.9", "0.9", "", false},
		{"example.com/other@sha256:abc", "0.9", "0.9", false},
	} {
		f := newEngine(t)
		f.oldImage, f.label = tc.image, tc.label
		d := startEngine(t, f)
		sock := filepath.Join(t.TempDir(), "present")
		_ = os.WriteFile(sock, nil, 0o600)
		// findSelf looks the container up by hostname: name the fake one so.
		host, _ := os.Hostname()
		f.names[host] = "minion-hub-1"
		delete(f.names, "old")
		f.running[host] = true
		_, why := Detect(context.Background(), d, sock, tc.channel)
		if (why == "") != tc.ok {
			t.Errorf("%s on %s: reason %q", tc.image, tc.channel, why)
		}
	}
}

func TestLockExcludesASecondUpdateAndStaleLocksAreTaken(t *testing.T) {
	f := newEngine(t)
	d := startEngine(t, f)
	ctx := context.Background()
	if err := acquireLock(ctx, d, f.dataDir); err != nil {
		t.Fatal(err)
	}
	// A preparation in progress (pulling, no helper yet) is not stale.
	if err := acquireLock(ctx, d, f.dataDir); err != ErrBusy {
		t.Fatalf("second lock during preparation: %v", err)
	}
	lock := filepath.Join(f.dataDir, LockFile)
	// A lock naming a helper that still exists is not stale.
	_ = os.WriteFile(lock, []byte("old"), 0o600)
	if err := acquireLock(ctx, d, f.dataDir); err != ErrBusy {
		t.Fatalf("lock of a live helper: %v", err)
	}
	// A lock naming a helper that is gone is stale and taken.
	_ = os.WriteFile(lock, []byte("gone"), 0o600)
	if err := acquireLock(ctx, d, f.dataDir); err != nil {
		t.Fatalf("lock of a gone helper: %v", err)
	}
	// A preparation lock older than any preparation is stale too.
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(lock, old, old)
	if err := acquireLock(ctx, d, f.dataDir); err != nil {
		t.Fatalf("old preparation lock: %v", err)
	}
}

// Data in the container's own layer would go with the container: refused.
func TestDataOutsideAMountIsRefused(t *testing.T) {
	f := newEngine(t)
	f.noMount = true
	d := startEngine(t, f)
	sock := filepath.Join(t.TempDir(), "present")
	_ = os.WriteFile(sock, nil, 0o600)
	host, _ := os.Hostname()
	f.names[host] = "minion-hub-1"
	f.running[host] = true
	if _, why := Detect(context.Background(), d, sock, "0.9"); !strings.Contains(why, "not on a volume or bind mount") {
		t.Fatalf("reason %q", why)
	}
}

// A tmpfs below the bind mount holds the data in memory only: refused.
func TestDataOnANestedTmpfsIsRefused(t *testing.T) {
	f := newEngine(t)
	f.tmpfs = true
	d := startEngine(t, f)
	sock := filepath.Join(t.TempDir(), "present")
	_ = os.WriteFile(sock, nil, 0o600)
	host, _ := os.Hostname()
	f.names[host] = "minion-hub-1"
	f.running[host] = true
	f.oldImage = "ghcr.io/patrick-1984/minion:0.9"
	// Point MINION_DATA_DIR below the bind, into the tmpfs.
	c, err := d.Inspect(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if onMount(c, f.dataDir+"/hub") {
		t.Fatal("a data directory on a tmpfs counts as shared")
	}
	if !onMount(c, f.dataDir) {
		t.Fatal("the bind mount itself is not shared")
	}
}

// The helper refuses before stopping the Hub when it cannot see the database.
func TestReplaceRefusesBeforeStoppingWithoutTheDatabase(t *testing.T) {
	f := newEngine(t)
	d := startEngine(t, f)
	_ = os.Remove(filepath.Join(f.dataDir, "minion.db"))
	if err := Replace(context.Background(), d, params(f, true)); err == nil {
		t.Fatal("no error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running["old"] || f.created != nil {
		t.Fatalf("old running=%v created=%v", f.running["old"], f.created != nil)
	}
	for _, c := range f.calls {
		if strings.HasSuffix(c, "/stop") {
			t.Fatalf("the Hub was stopped: %v", f.calls)
		}
	}
}

// A link in the data directory (it may be the database) is not copied
// silently: the backup fails, nothing is restored, the old Hub runs again.
func TestReplaceRefusesLinksInTheDataDirectory(t *testing.T) {
	f := newEngine(t)
	d := startEngine(t, f)
	elsewhere := filepath.Join(t.TempDir(), "real.db")
	_ = os.WriteFile(elsewhere, []byte("real"), 0o600)
	if err := os.Symlink(elsewhere, filepath.Join(f.dataDir, "other.db")); err != nil {
		t.Skip(err)
	}
	if err := Replace(context.Background(), d, params(f, true)); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running["old"] || f.created != nil {
		t.Fatalf("old running=%v created=%v", f.running["old"], f.created != nil)
	}
	if _, err := os.Lstat(filepath.Join(f.dataDir, "other.db")); err != nil {
		t.Errorf("the link was touched: %v", err)
	}
}

// A data directory without the database is not backed up as if empty.
func TestCopyDirNeedsTheDatabase(t *testing.T) {
	src := filepath.Join(t.TempDir(), "missing")
	if err := copyDir(src, filepath.Join(src, ".pre")); err == nil {
		t.Fatal("an absent directory was copied")
	}
	if _, err := os.Stat(src); err == nil {
		t.Fatal("the copy created the absent source")
	}
}
