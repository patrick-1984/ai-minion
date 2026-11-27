//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// WP12 (BRD minion-file-transfer v2, 5.6, files.v1): a session on one Worker
// pulls from and pushes to a session on another Worker. Its file authority
// comes only from a Verified source: the incarnation of a bound lease on the
// master lane, here a direct grant bound to that incarnation. The Worker side
// of the master lane is played by the test on Worker A's real Hub link,
// exactly as the master broker of the daemon sends it (master.open and
// master.report on an ev stream, master_call_v1 on a call stream); the caller
// end is the real caller engine (agent.CallerPull / agent.CallerPush) over
// the folder of A's session, with its data leg an fx stream A opens on the
// same connection and the receipt of a pull on A's call stream, as the
// daemon's caller (caller_files_daemon.go) runs it.

// fxMasterLane is the master lane of one candidate session of a Worker.
type fxMasterLane struct {
	t           *testing.T
	w           *fxWorker
	session     string
	incarnation string

	mu      sync.Mutex
	binding proto.MasterBinding
}

// startMasterLane opens the master lane on w's link and reports session as
// a verified candidate with a new incarnation, every two seconds.
func (e *fxE2E) startMasterLane(w *fxWorker, session string) *fxMasterLane {
	t := e.t
	t.Helper()
	m := &fxMasterLane{t: t, w: w, session: session, incarnation: "inc-" + strings.ReplaceAll(ops.NewUUIDv7(), "-", "")[:20]}
	waitFor(t, "the master link", func() bool { l := w.link.Load(); return l != nil && l.Welcome.MasterSessionV1 && l.Welcome.FilesV1 })
	l := w.link.Load()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := l.Session.OpenEvents(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); st.Reset(); <-done })
	candidates := []proto.MasterCandidate{{SessionName: session, IncarnationID: m.incarnation, Adapter: proto.MasterAdapterClaude,
		CatalogResult: proto.MasterCatalogEmpty}}
	report := func(seq uint64) error {
		typ := proto.TypeMasterReport
		if seq == 1 {
			typ = proto.TypeMasterOpen
		}
		_ = st.SetWriteDeadline(time.Now().Add(2 * time.Second))
		return st.WriteFrame(proto.MasterReport{Type: typ, Seq: seq, Candidates: candidates})
	}
	if err := report(1); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(done)
		for {
			body, err := st.ReadFrame()
			if err != nil {
				return
			}
			var state proto.MasterState
			if json.Unmarshal(body, &state) != nil {
				return
			}
			m.mu.Lock()
			m.binding = proto.MasterBinding{}
			for _, b := range state.Active {
				if b.IncarnationID == m.incarnation {
					m.binding = b
				}
			}
			m.mu.Unlock()
		}
	}()
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for seq := uint64(2); ; seq++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if report(seq) != nil {
				return
			}
		}
	}()
	return m
}

// lease returns the current lease of the candidate (zero without one).
func (m *fxMasterLane) lease() proto.MasterBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.binding
}

// call sends one master_call_v1 of operation on A's link and decodes the
// result into out.
func (m *fxMasterLane) call(ctx context.Context, operation string, args, out any) error {
	b := m.lease()
	if b.LeaseID == "" {
		return proto.NewError(proto.CodeForbidden, "no lease")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return callStream(ctx, m.w, proto.MasterCallRequest{Type: proto.TypeOpRequest, Op: proto.OpMasterCallV1, RequestID: ops.NewUUIDv7(),
		Args: proto.MasterCallArgs{IncarnationID: b.IncarnationID, Generation: b.Generation, LeaseID: b.LeaseID,
			Operation: operation, Arguments: raw}}, out)
}

// callStream sends one request frame on a new call stream of w's link and
// decodes an op.result into out; an error frame is returned as its error.
func callStream(ctx context.Context, w *fxWorker, frame, out any) error {
	st, err := w.link.Load().Session.OpenCall(ctx)
	if err != nil {
		return err
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(20 * time.Second))
	if err := st.WriteFrame(frame); err != nil {
		return err
	}
	body, err := st.ReadFrame()
	if err != nil {
		return err
	}
	var head struct {
		Type    string          `json:"type"`
		Result  json.RawMessage `json:"result"`
		Code    proto.Code      `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return err
	}
	if head.Type != proto.TypeOpResult {
		return proto.NewError(head.Code, head.Message)
	}
	if out != nil {
		return json.Unmarshal(head.Result, out)
	}
	return nil
}

// claimedCall sends op with source_session on an ordinary call stream: the
// path a daemon without a verified lease takes (a Claimed source).
func claimedCall(ctx context.Context, w *fxWorker, session, op string, args any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return callStream(ctx, w, map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": ops.NewUUIDv7(),
		"args": json.RawMessage(raw), "source_session": session}, nil)
}

// sessionCaller is the caller end of A's session over the master lane m.
func sessionCaller(t *testing.T, m *fxMasterLane) agent.FileCaller {
	t.Helper()
	root, err := filexfer.OpenDirRoot(m.w.dirs[m.session], filexfer.RootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return agent.FileCaller{
		Root: root,
		Admit: func(ctx context.Context, op string, args any) (ops.FileTransferResult, error) {
			var res ops.FileTransferResult
			err := m.call(ctx, op, args, &res)
			return res, err
		},
		Leg: func(ctx context.Context, adm ops.FileTransferResult) (*agent.FileLeg, error) {
			lo, ok := m.w.link.Load().Session.(interface {
				OpenLeg(context.Context) (*transport.LegConn, error)
			})
			if !ok {
				return nil, proto.NewError(proto.CodeUnsupportedOp, "no data legs")
			}
			conn, err := lo.OpenLeg(ctx)
			if err != nil {
				t.Logf("open the data leg: %v", err)
				return nil, err
			}
			return &agent.FileLeg{Client: &http.Client{Transport: transport.NewLegTransport(conn)},
				URL: transport.LegURL(proto.FilesLegPath(adm.TransferID)), Close: func() { _ = conn.Close() }}, nil
		},
		Receipt: func(ctx context.Context, a ops.FileReceiptArgs) {
			if err := callStream(ctx, m.w, map[string]any{"type": proto.TypeOpRequest, "op": ops.OpFileReceipt,
				"request_id": ops.NewUUIDv7(), "args": a}, nil); err != nil {
				t.Errorf("file_receipt on the call stream: %v", err)
			}
		},
		MinFreeBytes: -1,
	}
}

// grantSet creates or changes the grant A:boss -> B:build at level control.
func (e *fxE2E) grantSet(a, b *fxWorker, id string, rev uint64, pull, push *bool) ops.Grant {
	e.t.Helper()
	args := ops.GrantSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ID: id, IfRevision: rev,
		SourceWorkerID: a.id, SourceSession: "boss", TargetWorkerID: b.id, TargetSession: "build", Level: ops.GrantLevelControl,
		FilesPull: pull, FilesPush: push}
	code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpGrantSet, args, e.owner)
	var out ops.GrantSetResult
	if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.Grant.ID == "" {
		e.t.Fatalf("grant_set: %d %s", code, body)
	}
	return out.Grant
}

func TestFilesSessionCallerVerifiedSource(t *testing.T) {
	e := startFilesE2E(t)
	a := e.startWorker("source-box", fxWorkerOptions{master: true}, "boss")
	// B keeps its link too, to send a receipt that is not its own.
	b := e.startWorker("build-box", fxWorkerOptions{master: true}, "build")
	m := e.startMasterLane(a, "boss")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tgt := target(b, "build")
	content := pattern(300 << 10)
	writeFile(t, b.dirs["build"], "dist/app.txt", content)
	writeFile(t, a.dirs["boss"], "fix.patch", []byte("the fix\n"))

	// A session without any grant gets nothing on the ordinary call stream.
	err := claimedCall(ctx, a, "boss", ops.OpFilePull, ops.FilePullArgs{Target: tgt, Path: "dist/app.txt"})
	if codeOf(err) != proto.CodeForbidden {
		t.Fatalf("claimed pull = %v", err)
	}
	if err := claimedCall(ctx, a, "boss", ops.OpFileList, ops.FileListArgs{Target: tgt}); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("claimed list = %v", err)
	}

	// A grant bound to the incarnation, without file flags: a verified lease,
	// but no file authority.
	g := e.grantSet(a, b, "", 0, nil, nil)
	bind := true
	code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpGrantBind, ops.GrantBindArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		ID: g.ID, IfRevision: g.Revision, Bind: &bind}, e.owner)
	if code != http.StatusOK {
		t.Fatalf("grant_bind: %d %s", code, body)
	}
	var bound ops.GrantSetResult
	if err := json.Unmarshal([]byte(body), &bound); err != nil || bound.Grant.Revision == 0 {
		t.Fatalf("grant_bind answer %s: %v", body, err)
	}
	waitFor(t, "the lease of the bound grant", func() bool { return m.lease().LeaseID != "" })
	caller := sessionCaller(t, m)
	if _, err := agent.CallerPull(ctx, caller, ops.FilePullArgs{Target: tgt, Path: "dist/app.txt"}); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("pull without files_pull = %v", err)
	}
	if err := m.call(ctx, ops.OpFileList, ops.FileListArgs{Target: tgt}, nil); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("list without files_pull = %v", err)
	}
	if exists(a.dirs["boss"], "app.txt") {
		t.Fatal("a refused pull published")
	}

	// files_pull only: list and pull, push refused.
	yes, no := true, false
	g = e.grantSet(a, b, g.ID, bound.Grant.Revision, &yes, &no)
	var list ops.FileListResult
	if err := m.call(ctx, ops.OpFileList, ops.FileListArgs{Target: tgt, Path: "dist"}, &list); err != nil {
		t.Fatalf("list with files_pull: %v", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Name != "app.txt" {
		t.Fatalf("list = %+v", list)
	}
	res, err := agent.CallerPull(ctx, caller, ops.FilePullArgs{Target: tgt, Path: "dist/app.txt"})
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pull with files_pull = %+v, %v", res, err)
	}
	if got := readFile(t, a.dirs["boss"], "app.txt"); !bytes.Equal(got, content) {
		t.Fatal("the pulled file differs")
	}
	waitFor(t, "the receipt of the pull", func() bool { return e.transfer(res.TransferID).State == ops.FileStateDelivered })
	pullID := res.TransferID
	rec := e.transfer(pullID)
	if rec.SourceWorkerID != a.id || rec.SourceSession != "boss" || rec.GrantID != g.ID {
		t.Fatalf("pull record = %+v", rec)
	}
	if _, err := agent.CallerPush(ctx, caller, ops.FilePushArgs{Target: tgt, Path: "fix.patch", As: "patches/fix.patch"}); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("push without files_push = %v", err)
	}

	// files_push too: the push lands in B's session.
	g = e.grantSet(a, b, g.ID, g.Revision, nil, &yes)
	res, err = agent.CallerPush(ctx, caller, ops.FilePushArgs{Target: tgt, Path: "fix.patch", As: "fix.patch"})
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("push with files_push = %+v, %v", res, err)
	}
	if got := readFile(t, b.dirs["build"], "fix.patch"); string(got) != "the fix\n" {
		t.Fatalf("pushed content = %q", got)
	}
	if rec := e.transfer(res.TransferID); rec.State != ops.FileStateDelivered || rec.SourceSession != "boss" {
		t.Fatalf("push record = %+v", rec)
	}

	// A receipt is bound to the receiving Worker and its leg token: the
	// target Worker cannot send one for the pull.
	err = callStream(ctx, b, map[string]any{"type": proto.TypeOpRequest, "op": ops.OpFileReceipt, "request_id": ops.NewUUIDv7(),
		"args": ops.FileReceiptArgs{TransferID: pullID, LegToken: strings.Repeat("A", 43),
			FileReceipt: ops.FileReceipt{Outcome: ops.OutcomeFailedNotSent}}}, nil)
	if codeOf(err) != proto.CodeForbidden && codeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("a receipt of another Worker = %v", err)
	}
	if e.transfer(pullID).State != ops.FileStateDelivered {
		t.Fatal("a foreign receipt changed the record")
	}

	// Revoking the grant ends the lease: nothing is admitted any more.
	code, body = e.ph.do("POST", "/api/v1/ops/"+ops.OpGrantRevoke, ops.GrantRevokeArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ID: g.ID, IfRevision: g.Revision}, e.owner)
	if code != http.StatusOK {
		t.Fatalf("grant_revoke: %d %s", code, body)
	}
	waitFor(t, "the lease to end", func() bool { return m.lease().LeaseID == "" })
	if _, err := agent.CallerPull(ctx, caller, ops.FilePullArgs{Target: tgt, Path: "dist/app.txt", To: "again.txt"}); err == nil {
		t.Fatal("a pull after the revoke was admitted")
	}
}
