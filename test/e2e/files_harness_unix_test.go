//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// WP11 (BRD minion-file-transfer v2, files.v1): the harness of the file
// transfer suites. A real default Hub (startShareHub: REST dispatcher, authz,
// the files module with its public data-leg endpoint, the router, events)
// and real Workers: the real connector over the real handshake, op server,
// executor with the file transfer end (agent.Files) over a real tmux server
// on a private socket, whose panes run in t.TempDir() folders (the pane
// folder is the transfer root, proven from /proc and tmux). The caller end
// is the real caller engine (agent.CallerPull / agent.CallerPush) with the
// REST admission and the Hub's public leg endpoint, exactly as the REST CLI
// (`minion files pull|push` with a Hub configured) runs it.

// fxWorker is one started Worker with a file transfer end.
type fxWorker struct {
	id    string
	name  string
	tmux  *tmuxtest.Server
	ex    *agent.Executor
	files *agent.Files
	mode  atomic.Value // string: the local switch file_transfer
	// ahead moves the Worker's file clock forward (a stable file is one
	// not changed for proto.StableFileAge: resume tests).
	ahead atomic.Int64
	dirs  map[string]string
	stop  func()
}

// setMode sets the Worker's local switch (ops.FileMode*).
func (w *fxWorker) setMode(m string) { w.mode.Store(m) }

// fxE2E is the Hub with its owner and the started Workers.
type fxE2E struct {
	t       *testing.T
	ph      *shareHub
	ownerID string
	owner   map[string]string
	// bulk is an HTTP client without an overall timeout, for data legs.
	bulk *http.Client
}

func startFilesE2E(t *testing.T) *fxE2E {
	t.Helper()
	// The tests run as a test binary that cannot be its own probe child:
	// the resolution is set as the probe would on Linux, or the walk.
	filexfer.SetDefaultResolveMode(filexfer.ResolveWalk)
	if runtime.GOOS == "linux" {
		filexfer.SetDefaultResolveMode(filexfer.ResolveOpenat2)
	}
	e := &fxE2E{t: t, ph: startShareHub(t)}
	se := &shareE2E{t: t, ph: e.ph}
	e.ownerID, e.owner = se.user("olivia")
	e.bulk = &http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test Hub
	return e
}

// fxWorkerOptions shape a started Worker.
type fxWorkerOptions struct {
	// noFiles starts a Worker without a file transfer end: it announces
	// neither the file ops nor files.v1, as a v0.23 Worker.
	noFiles bool
}

// startWorker starts a Worker named name owned by the owner, with one tmux
// session per entry of sessions, each running in a folder of its own.
func (e *fxE2E) startWorker(name string, opt fxWorkerOptions, sessions ...string) *fxWorker {
	t := e.t
	t.Helper()
	w := &fxWorker{name: name, dirs: map[string]string{}}
	w.setMode(ops.FileModeOn)
	dir := t.TempDir()
	st, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveShare(wstore.ShareSelection{Mode: wstore.ShareAll}); err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), agent.Enrollment{Pin: identity.Pin{Pub: e.ph.wsv.Identity().Public()}}); err != nil {
		t.Fatal(err)
	}
	w.id = store.NewID()
	if err := e.ph.st.InsertWorker(context.Background(), nil, store.WorkerRow{ID: w.id, Pub: key.Public(), Name: name, OwnerID: e.ownerID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	w.tmux = tmuxtest.New(t)
	for _, s := range sessions {
		d := realDir(t, t.TempDir())
		w.dirs[s] = d
		// The pane process runs in its folder: the root proof reads
		// /proc/<pid>/cwd and tmux's pane_current_path.
		w.tmux.NewSession(s, "sh", "-c", "cd '"+d+"' && exec cat")
	}
	host, err := tmux.New(w.tmux.Socket())
	if err != nil {
		t.Fatal(err)
	}
	policy := agent.NewPolicy(st)
	w.ex, err = agent.NewExecutor(agent.Config{Host: host, Classifier: agentdetect.New(), State: st, Policy: policy, LockWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !opt.noFiles {
		w.files = agent.NewFiles(w.ex, agent.FilesConfig{
			Mode:         func() string { return w.mode.Load().(string) },
			Floor:        func() *filexfer.Floor { return filexfer.NewFloor(filexfer.FloorConfig{Home: dir, StateDir: dir}) },
			StateDir:     dir,
			MinFreeBytes: -1,
			Now:          func() time.Time { return time.Now().Add(time.Duration(w.ahead.Load())) },
		})
		w.ex.AttachFiles(w.files)
		if st := w.files.Status(); st != "" {
			t.Fatalf("the Worker has no file transfer end: %s", st)
		}
	}
	srv, err := agent.NewOpServer(agent.OpServerConfig{Executor: w.ex, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := agent.NewHostObserver(agent.HostObserverConfig{Host: host, Classifier: agentdetect.New(), Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := agent.NewEventPublisher(agent.EventConfig{Observer: obs, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := agent.NewConnector(agent.ConnectorConfig{
		HubURL: "https://" + e.ph.addr, Name: name, Ops: w.ex.Ops, Features: w.ex.Features, Key: enr.LoadKey, Enrollment: enr, Policy: policy,
		Dialer:   agent.NewHubDialer(),
		Backoff:  &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.OnLink("op-server", srv.Serve)
	conn.OnLink("events", pub.Serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	stopped := false
	w.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
		_ = srv.Stop(context.Background())
	}
	t.Cleanup(w.stop)
	waitFor(t, "the Worker online", func() bool { return e.ph.wsv.Registry().IsOnline(w.id) })
	waitFor(t, "the sessions in the Hub", func() bool {
		ws, ok := e.ph.events.Sessions(w.id)
		return ok && len(ws.Sessions) == len(sessions)
	})
	return w
}

func realDir(t *testing.T, d string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// key creates an API key of the owner on the Workers with the actions.
func (e *fxE2E) key(name string, actions []ops.Action, workers ...*fxWorker) string {
	e.t.Helper()
	res := make([]ops.KeyResource, 0, len(workers))
	for _, w := range workers {
		res = append(res, ops.KeyResource{Kind: ops.KeyResourceWorker, WorkerID: w.id})
	}
	code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpAPIKeyCreate, ops.APIKeyCreateArgs{Name: name, Resources: res, Actions: actions}, e.owner)
	if code != http.StatusOK {
		e.t.Fatalf("apikey_create: %d %s", code, body)
	}
	var out ops.APIKeyCreateResult
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Key == "" {
		e.t.Fatalf("apikey_create answer %s: %v", body, err)
	}
	return out.Key
}

var fileActions = []ops.Action{ops.ActionRead, ops.ActionFileRead, ops.ActionFileWrite}

// restError turns a REST error answer into the *proto.Error it carries.
func restError(status int, body string) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &env)
	if env.Error.Code == "" {
		return proto.NewError(proto.CodeInternal, "status "+http.StatusText(status)+": "+body)
	}
	return proto.NewError(proto.Code(env.Error.Code), env.Error.Message)
}

// call sends one REST op with the bearer and decodes a 200 answer into out.
func (e *fxE2E) call(bearer, op string, args, out any) error {
	e.t.Helper()
	code, body := e.ph.do("POST", "/api/v1/ops/"+op, args, asKey(bearer))
	if code != http.StatusOK {
		return restError(code, body)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(body), out); err != nil {
			e.t.Fatalf("%s answer %s: %v", op, body, err)
		}
	}
	return nil
}

// fxCaller is a REST caller end over a folder of the test.
type fxCaller struct {
	agent.FileCaller
	dir string
	// receiptErr is the error of the last receipt that failed.
	receiptErr atomic.Pointer[error]
}

// caller returns the caller end of the REST CLI with bearer over a new
// folder; mut changes it (a leg that cuts the body, for example).
func (e *fxE2E) caller(bearer string) *fxCaller {
	t := e.t
	t.Helper()
	dir := realDir(t, t.TempDir())
	root, err := filexfer.OpenDirRoot(dir, filexfer.RootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	c := &fxCaller{dir: dir}
	c.FileCaller = agent.FileCaller{
		Root: root,
		Admit: func(ctx context.Context, op string, args any) (ops.FileTransferResult, error) {
			var res ops.FileTransferResult
			err := e.call(bearer, op, args, &res)
			return res, err
		},
		Leg: func(ctx context.Context, adm ops.FileTransferResult) (*agent.FileLeg, error) {
			return &agent.FileLeg{Client: e.bulk, URL: "https://" + e.ph.addr + proto.FilesLegPath(adm.TransferID),
				Header: http.Header{"Authorization": {"Bearer " + bearer}}, Close: func() {}}, nil
		},
		Receipt: func(ctx context.Context, a ops.FileReceiptArgs) {
			if err := e.call(bearer, ops.OpFileReceipt, a, nil); err != nil {
				c.receiptErr.Store(&err)
			}
		},
		MinFreeBytes: -1,
	}
	return c
}

func target(w *fxWorker, session string) ops.Target {
	return ops.Target{WorkerID: w.id, SessionName: session}
}

// writeFile writes content at rel under dir.
func writeFile(t *testing.T, dir, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(dir, rel string) bool {
	_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// pattern returns n bytes that compress (a repeating text with a counter).
func pattern(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		b.WriteString("line ")
		b.WriteString(hex.EncodeToString([]byte{byte(i >> 16), byte(i >> 8), byte(i)}))
		b.WriteString(" of the test content\n")
	}
	return b.Bytes()[:n]
}

// noise returns n bytes that do not compress.
func noise(n int) []byte {
	out := make([]byte, 0, n+32)
	s := sha256.Sum256([]byte("seed"))
	for len(out) < n {
		out = append(out, s[:]...)
		s = sha256.Sum256(s[:])
	}
	return out[:n]
}

// codeOf returns the proto code of err ("" for nil).
func codeOf(err error) proto.Code {
	if err == nil {
		return ""
	}
	return proto.CodeOf(err)
}

// transfers lists the owner's transfer history on the Hub.
func (e *fxE2E) transfers() []ops.FileTransferRecord {
	e.t.Helper()
	code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpFileTransfersList, map[string]any{}, e.owner)
	if code != http.StatusOK {
		e.t.Fatalf("file_transfers_list: %d %s", code, body)
	}
	var out ops.FileTransfersListResult
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatal(err)
	}
	return out.Transfers
}

// transfer returns the history record of id.
func (e *fxE2E) transfer(id string) ops.FileTransferRecord {
	e.t.Helper()
	for _, r := range e.transfers() {
		if r.TransferID == id {
			return r
		}
	}
	e.t.Fatalf("transfer %s is not in the history", id)
	return ops.FileTransferRecord{}
}
