package files

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// memStore is an in-memory Store with the state rules of the Hub store.
type memStore struct {
	mu        sync.Mutex
	rows      map[string]store.FileTransferRow
	entries   map[string][]ops.FileTransferEntry
	failWrite error // InsertFileTransfer of an admitted record fails with it
	inserts   int
}

func newMemStore() *memStore {
	return &memStore{rows: map[string]store.FileTransferRow{}, entries: map[string][]ops.FileTransferEntry{}}
}

func (m *memStore) InsertFileTransfer(_ context.Context, r store.FileTransferRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.State == ops.FileStateAdmitted && m.failWrite != nil {
		return m.failWrite
	}
	if _, ok := m.rows[r.ID]; ok {
		return store.ErrFileTransferExists
	}
	if r.State == ops.FileStateDenied {
		ended := r.CreatedAt
		r.EndedAt, r.DeadlineAt = &ended, nil
	}
	m.inserts++
	m.rows[r.ID] = r
	return nil
}

func (m *memStore) FileTransfer(_ context.Context, id string) (store.FileTransferRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return r, store.ErrNotFound
	}
	return r, nil
}

func (m *memStore) row(id string) store.FileTransferRow {
	r, _ := m.FileTransfer(context.Background(), id)
	return r
}

func (m *memStore) MarkFileTransferPrepared(_ context.Context, id string, p store.FilePrepared, now int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return store.ErrNotFound
	}
	if r.State != ops.FileStateAdmitted {
		return store.ErrFileTransferState
	}
	r.State, r.TargetRoot, r.Kind, r.Bytes, r.UpdatedAt = ops.FileStatePrepared, p.TargetRoot, p.Kind, p.Bytes, now
	if p.DeadlineAt != nil {
		r.DeadlineAt = p.DeadlineAt
	}
	m.rows[id] = r
	return nil
}

func (m *memStore) FinishFileTransfer(_ context.Context, id string, out store.FileOutcome, now int64) (store.FileTransferRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return r, false, store.ErrNotFound
	}
	open := r.State == ops.FileStateAdmitted || r.State == ops.FileStatePrepared
	if !open && r.State != ops.FileStateUnknown || r.State == ops.FileStateUnknown && out.State == ops.FileStateUnknown {
		return r, false, nil
	}
	r.State, r.Reason, r.Files, r.Bytes, r.WireBytes = out.State, out.Reason, out.Files, out.Bytes, out.WireBytes
	r.ManifestDigest, r.Protected, r.EntriesOmitted = out.ManifestDigest, out.Protected, out.EntriesOmitted
	r.DeadlineAt, r.EndedAt = nil, &now
	m.rows[id] = r
	m.entries[id] = out.Entries
	return r, true, nil
}

func (m *memStore) FileTransferEntries(_ context.Context, id string) ([]ops.FileTransferEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[id], nil
}

func (m *memStore) ReconcileFileTransfers(_ context.Context, now int64) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, r := range m.rows {
		if (r.State == ops.FileStateAdmitted || r.State == ops.FileStatePrepared) && r.DeadlineAt != nil && *r.DeadlineAt < now {
			r.State, r.DeadlineAt, r.EndedAt = ops.FileStateUnknown, nil, &now
			m.rows[id] = r
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (m *memStore) PurgeFileTransfers(_ context.Context, before int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, r := range m.rows {
		if r.CreatedAt < before {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

func (m *memStore) ListFileTransfers(_ context.Context, f store.FileTransferFilter) ([]store.FileTransferRow, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.FileTransferRow
	for _, r := range m.rows {
		if f.Admin || r.SourceOwnerID == f.ViewerID || r.TargetOwnerID == f.ViewerID || r.PrincipalUserID == f.ViewerID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, "", nil
}

func (m *memStore) WorkerFileMode(context.Context, string) (string, error) {
	return ops.FileModeOn, nil
}
func (m *memStore) WorkerPreviewOnly(context.Context, string) (bool, error) { return false, nil }
func (m *memStore) FileAcceptances(context.Context, string) ([]store.AcceptRow, error) {
	return nil, nil
}

// switchDecider allows while allowed is true; calls counts the decisions.
type switchDecider struct {
	allowed atomic.Bool
	calls   atomic.Int64
	owner   string
}

func (d *switchDecider) DecideFile(_ context.Context, p authz.Principal, _ string, _ authz.SessionResource) (string, authz.FileBasis, error) {
	d.calls.Add(1)
	if !d.allowed.Load() {
		return "", authz.FileBasis{}, proto.NewError(proto.CodeForbidden, "not allowed")
	}
	return d.owner, authz.FileBasis{Standing: true}, nil
}

// memAudit keeps the audit events.
type memAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (a *memAudit) Record(_ context.Context, ev audit.Event) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	a.events = append(a.events, ev)
	a.mu.Unlock()
	return nil
}

func (a *memAudit) all() []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]audit.Event(nil), a.events...)
}

// fakeWorker is a target Worker: a Hub session and a Worker session over
// net.Pipe with data legs, whose Worker end serves each inbound leg with h.
type fakeWorker struct {
	hub    *transport.Session
	conn   []byte
	opened atomic.Int64
}

func (f *fakeWorker) OpenLeg(ctx context.Context, _ string, conn []byte) (*transport.LegConn, error) {
	if string(conn) != string(f.conn) {
		return nil, errors.New("another connection")
	}
	f.opened.Add(1)
	return f.hub.OpenLeg(ctx)
}

func newFakeWorker(t *testing.T, h http.Handler) *fakeWorker {
	t.Helper()
	a, b := net.Pipe()
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{Files: true})
	if err != nil {
		t.Fatal(err)
	}
	wk, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{Files: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = hub.Close(); _ = wk.Close() })
	go func() {
		for {
			st, err := wk.Accept(ctx)
			if err != nil {
				return
			}
			go func() { _ = transport.ServeLeg(ctx, transport.NewLegServer(h), transport.NewLegConn(st)) }()
		}
	}()
	return &fakeWorker{hub: hub, conn: []byte("target-conn")}
}

const (
	targetWorker = "01900000-0000-7000-8000-000000000001"
	callerUser   = "01900000-0000-7000-8000-0000000000aa"
	keyID        = "01900000-0000-7000-8000-0000000000bb"
)

type harness struct {
	t   *testing.T
	st  *memStore
	az  *switchDecider
	aud *memAudit
	wk  *fakeWorker
	svc *Service
	srv *httptest.Server
	who authz.Principal
}

func newHarness(t *testing.T, h http.Handler, tune func(*Config)) *harness {
	t.Helper()
	hs := &harness{t: t, st: newMemStore(), az: &switchDecider{owner: callerUser}, aud: &memAudit{}}
	hs.az.allowed.Store(true)
	hs.wk = newFakeWorker(t, h)
	hs.who = authz.NewAPIKeyPrincipal(callerUser, nil, "").WithKeyID(keyID)
	cfg := Config{
		Store: hs.st, Authz: hs.az, Legs: hs.wk, Audit: hs.aud,
		Conn: func(string) []byte { return hs.wk.conn },
	}
	if tune != nil {
		tune(&cfg)
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hs.svc = svc
	hs.srv = httptest.NewServer(svc.Handler(func(*http.Request) (authz.Principal, error) { return hs.who, nil }))
	t.Cleanup(hs.srv.Close)
	return hs
}

// admit admits a transfer as the target Worker would answer it and returns
// the caller's admission.
func (hs *harness) admit(op string, args any, answer ops.FileTransferResult) (ops.FileTransferResult, workers.FileDispatch) {
	hs.t.Helper()
	d, err := hs.svc.AdmitFile(context.Background(), workers.FileCall{
		Principal: hs.who, Op: op, WorkerID: targetWorker, Session: "build", Args: args,
	})
	if err != nil {
		hs.t.Fatalf("admission: %v", err)
	}
	res, err := d.Finish(context.Background(), answer, nil)
	if err != nil {
		hs.t.Fatalf("admission answer: %v", err)
	}
	return res.(ops.FileTransferResult), d
}

func fileAnswer() ops.FileTransferResult {
	return ops.FileTransferResult{Stage: ops.FileStageAdmitted, Kind: ops.FileKindFile, Size: 11, TargetRoot: "~/app"}
}
