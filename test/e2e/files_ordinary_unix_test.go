//go:build e2e && unix

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// ordinaryCall sends op with source_session on an ordinary call stream of w
// and decodes the result into out.
func ordinaryCall(ctx context.Context, w *fxWorker, session, op string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return callStream(ctx, w, map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": ops.NewUUIDv7(),
		"args": json.RawMessage(raw), "source_session": session}, out)
}

// ordinaryCaller is the caller end of session on w over its ordinary call
// stream: no master lease, no bound grant.
func ordinaryCaller(t *testing.T, w *fxWorker, session string) agent.FileCaller {
	t.Helper()
	root, err := filexfer.OpenDirRoot(w.dirs[session], filexfer.RootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return agent.FileCaller{
		Root: root,
		Admit: func(ctx context.Context, op string, args any) (ops.FileTransferResult, error) {
			var res ops.FileTransferResult
			err := ordinaryCall(ctx, w, session, op, args, &res)
			return res, err
		},
		Leg: func(ctx context.Context, adm ops.FileTransferResult) (*agent.FileLeg, error) {
			lo, ok := w.link.Load().Session.(interface {
				OpenLeg(context.Context) (*transport.LegConn, error)
			})
			if !ok {
				return nil, proto.NewError(proto.CodeUnsupportedOp, "no data legs")
			}
			conn, err := lo.OpenLeg(ctx)
			if err != nil {
				return nil, err
			}
			return &agent.FileLeg{Client: &http.Client{Transport: transport.NewLegTransport(conn)},
				URL: transport.LegURL(proto.FilesLegPath(adm.TransferID)), Close: func() { _ = conn.Close() }}, nil
		},
		Receipt: func(ctx context.Context, a ops.FileReceiptArgs) {
			if err := callStream(ctx, w, map[string]any{"type": proto.TypeOpRequest, "op": ops.OpFileReceipt,
				"request_id": ops.NewUUIDv7(), "args": a}, nil); err != nil {
				t.Errorf("file_receipt on the call stream: %v", err)
			}
		},
		MinFreeBytes: -1,
	}
}

// v0.26.0, the owner's report: one Add grant submission (source session
// wb-2, every shared session of the target Worker, Control, receive and send
// files) lets the ordinary session list, pull and push with no bind, no
// master and no lease; a revoke stops it.
func TestFilesOrdinarySessionOneStepGrant(t *testing.T) {
	e := startFilesE2E(t)
	a := e.startWorker("coder-box", fxWorkerOptions{master: true}, "wb-2", "other")
	b := e.startWorker("android-box", fxWorkerOptions{master: true}, "minion-5")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tgt := target(b, "minion-5")
	writeFile(t, b.dirs["minion-5"], "out/report.txt", []byte("report\n"))
	writeFile(t, a.dirs["wb-2"], "app.apk.txt", []byte("payload\n"))

	yes := true
	args := ops.GrantSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, SourceScope: ops.GrantSourceSession,
		SourceWorkerID: a.id, SourceSession: "wb-2", Targets: []ops.GrantTarget{{WorkerID: b.id}},
		Level: ops.GrantLevelControl, Note: "to android", FilesPull: &yes, FilesPush: &yes}
	code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpGrantSet, args, e.owner)
	var res ops.GrantSetResult
	if code != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil || len(res.CreatedIDs) != 1 || !res.Grant.FilesPush {
		t.Fatalf("grant_set: %d %s", code, body)
	}
	g := res.Grant

	var list ops.FileListResult
	if err := ordinaryCall(ctx, a, "wb-2", ops.OpFileList, ops.FileListArgs{Target: tgt, Path: "out"}, &list); err != nil {
		t.Fatalf("list from an ordinary session: %v", err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Name != "report.txt" {
		t.Fatalf("list = %+v", list)
	}
	caller := ordinaryCaller(t, a, "wb-2")
	pulled, err := agent.CallerPull(ctx, caller, ops.FilePullArgs{Target: tgt, Path: "out/report.txt"})
	if err != nil || pulled.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pull from an ordinary session = %+v, %v", pulled, err)
	}
	if got := readFile(t, a.dirs["wb-2"], "report.txt"); string(got) != "report\n" {
		t.Fatalf("pulled content = %q", got)
	}
	pushed, err := agent.CallerPush(ctx, caller, ops.FilePushArgs{Target: tgt, Path: "app.apk.txt", As: "app.apk.txt"})
	if err != nil || pushed.Outcome != ops.OutcomeDelivered {
		t.Fatalf("push from an ordinary session = %+v, %v", pushed, err)
	}
	if got := readFile(t, b.dirs["minion-5"], "app.apk.txt"); string(got) != "payload\n" {
		t.Fatalf("pushed content = %q", got)
	}
	if rec := e.transfer(pushed.TransferID); rec.SourceWorkerID != a.id || rec.SourceSession != "wb-2" || rec.GrantID != g.ID {
		t.Fatalf("push record = %+v", rec)
	}
	// Another session of the same Worker holds nothing.
	if err := ordinaryCall(ctx, a, "other", ops.OpFileList, ops.FileListArgs{Target: tgt}, nil); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("list from an unselected session = %v", err)
	}

	code, body = e.ph.do("POST", "/api/v1/ops/"+ops.OpGrantRevoke, ops.GrantRevokeArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		ID: g.ID, IfRevision: g.Revision}, e.owner)
	if code != http.StatusOK {
		t.Fatalf("grant_revoke: %d %s", code, body)
	}
	if _, err := agent.CallerPush(ctx, caller, ops.FilePushArgs{Target: tgt, Path: "app.apk.txt", As: "again.txt"}); err == nil {
		t.Fatal("a push after the revoke was admitted")
	}
	if exists(b.dirs["minion-5"], "again.txt") {
		t.Fatal("a refused push published")
	}
}
