package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var (
	content     = []byte("hello world")
	contentSum  = sha256.Sum256(content)
	contentHex  = hex.EncodeToString(contentSum[:])
	digestField = proto.FormatDigestField(contentSum[:])
)

// servePull is a target Worker's GET handler: the raw file with digest
// trailers, after checking the target leg's token. before runs after the
// body and before the trailers.
func servePull(token *string, before func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if *token == "" || r.Header.Get(proto.HeaderLegToken) != *token {
			legError(w, proto.CodeForbidden, "bad token")
			return
		}
		w.Header().Set("Trailer", proto.HeaderContentDigest+", "+proto.HeaderReprDigest)
		w.Header().Set("Content-Type", proto.ContentTypeFile)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if before != nil {
			before()
		}
		w.Header().Set(proto.HeaderContentDigest, digestField)
		w.Header().Set(proto.HeaderReprDigest, digestField)
	}
}

func get(t *testing.T, url, token string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set(proto.HeaderLegToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func waitState(t *testing.T, st *memStore, id, want string) store.FileTransferRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := st.row(id)
		if r.State == want {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("record state %q, want %q", r.State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func targetToken(t *testing.T, d workers.FileDispatch) string {
	t.Helper()
	switch a := d.Args.(type) {
	case ops.FilePullArgs:
		return a.LegToken
	case ops.FilePushArgs:
		return a.LegToken
	}
	t.Fatalf("routed arguments of type %T", d.Args)
	return ""
}

// A pull is recorded before anything is routed, the Hub sets its own
// members, the leg relays the body and the trailers after the gate, and the
// receiver's receipt ends it delivered.
func TestPullRelayAndReceipt(t *testing.T) {
	var tok string
	hs := newHarness(t, servePull(&tok, nil), nil)
	d, err := hs.svc.AdmitFile(context.Background(), workers.FileCall{
		Principal: hs.who, Op: ops.OpFilePull, WorkerID: targetWorker, Session: "build",
		Args: ops.FilePullArgs{Target: ops.Target{WorkerID: targetWorker, SessionName: "build"}, Path: "dist/a.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	routed := d.Args.(ops.FilePullArgs)
	if !ops.ValidUUIDv7(routed.TransferID) || !proto.ValidLegToken(routed.LegToken) || routed.MaxBytes != proto.MaxTransferBytes {
		t.Fatalf("Hub members not set: %+v", routed.FileLegArgs)
	}
	if r := hs.st.row(routed.TransferID); r.State != ops.FileStateAdmitted || r.Path != "dist/a.txt" {
		t.Fatalf("record before routing: %+v", r)
	}
	tok = routed.LegToken
	res, err := d.Finish(context.Background(), fileAnswer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	adm := res.(ops.FileTransferResult)
	if adm.LegToken == "" || adm.LegToken == tok || adm.TransferID != routed.TransferID || adm.ExpiresAt == nil {
		t.Fatalf("admission: %+v", adm)
	}
	resp, body, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) || resp.Trailer.Get(proto.HeaderContentDigest) != digestField {
		t.Fatalf("leg: %d %q trailers %v", resp.StatusCode, body, resp.Trailer)
	}
	// The token is single use.
	if resp, _, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken); err != nil || resp.StatusCode != http.StatusGone {
		t.Fatalf("second leg: %v %v", resp, err)
	}
	waitState(t, hs.st, adm.TransferID, ops.FileStatePrepared)
	rec := ops.FileReceiptArgs{TransferID: adm.TransferID, LegToken: adm.LegToken, FileReceipt: ops.FileReceipt{
		Outcome: ops.OutcomeDelivered, Files: 1, Bytes: int64(len(content)), ManifestDigest: contentHex,
	}}
	// Only the receiving identity with its token.
	if _, err := hs.svc.Receipt(context.Background(), "worker:"+targetWorker, rec); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("receipt of another identity: %v", err)
	}
	bad := rec
	bad.LegToken = tok
	if _, err := hs.svc.ReceiptFor(context.Background(), hs.who, bad); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("receipt with the wrong token: %v", err)
	}
	got, err := hs.svc.ReceiptFor(context.Background(), hs.who, rec)
	if err != nil || got.State != ops.FileStateDelivered {
		t.Fatalf("receipt: %+v %v", got, err)
	}
	again, err := hs.svc.ReceiptFor(context.Background(), hs.who, rec)
	if err != nil || again.State != ops.FileStateDelivered {
		t.Fatalf("repeated receipt: %+v %v", again, err)
	}
	r := hs.st.row(adm.TransferID)
	if r.WireBytes != int64(len(content)) || r.ManifestDigest != contentHex {
		t.Fatalf("record: %+v", r)
	}
	checkAudit(t, hs.aud.all())
}

// checkAudit checks that every event passes the detail grammar and names no
// path or token.
func checkAudit(t *testing.T, evs []audit.Event) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no audit events")
	}
	for _, ev := range evs {
		if err := ev.Validate(); err != nil || !audit.ValidDetail(ev.Detail) {
			t.Fatalf("audit event %+v: %v", ev, err)
		}
		if strings.Contains(ev.Detail, "dist") || strings.Contains(ev.Detail, "a.txt") || !strings.Contains(ev.Detail, "transfer=") {
			t.Fatalf("audit detail %q", ev.Detail)
		}
	}
}

// Revoking the authority after the body, before the trailers: the commit
// gate withholds the trailers and the record ends failed_not_sent.
func TestCommitGateWithholdsTrailers(t *testing.T) {
	var tok string
	var hs *harness
	hs = newHarness(t, servePull(&tok, func() { hs.az.allowed.Store(false) }), nil)
	adm, d := hs.admit(ops.OpFilePull, ops.FilePullArgs{Path: "a.txt"}, fileAnswer())
	tok = targetToken(t, d)
	resp, _, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken)
	if err == nil && resp.Trailer.Get(proto.HeaderContentDigest) != "" {
		t.Fatal("the trailers were forwarded after the revocation")
	}
	r := waitState(t, hs.st, adm.TransferID, ops.FileStateFailedNotSent)
	if r.Reason != string(proto.CodeForbidden) {
		t.Fatalf("reason %q", r.Reason)
	}
}

// A re-decision that refuses while bytes move resets both legs.
func TestRedecideResetsLegs(t *testing.T) {
	var tok string
	release := make(chan struct{})
	var hs *harness
	h := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		w.(http.Flusher).Flush()
		hs.az.allowed.Store(false)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	hs = newHarness(t, http.HandlerFunc(h), func(c *Config) { c.Redecide = 20 * time.Millisecond })
	defer close(release)
	adm, d := hs.admit(ops.OpFilePull, ops.FilePullArgs{Path: "a.txt"}, fileAnswer())
	tok = targetToken(t, d)
	_ = tok
	_, _, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken)
	if err == nil {
		t.Fatal("the caller leg ended normally after the revocation")
	}
	waitState(t, hs.st, adm.TransferID, ops.FileStateFailedNotSent)
}

// The record comes before any leg or routed request: an insert that fails
// refuses the transfer and nothing is routed.
func TestInsertFailureRefuses(t *testing.T) {
	hs := newHarness(t, http.NotFoundHandler(), nil)
	hs.st.failWrite = errors.New("disk full")
	_, err := hs.svc.AdmitFile(context.Background(), workers.FileCall{
		Principal: hs.who, Op: ops.OpFilePull, WorkerID: targetWorker, Session: "build", Args: ops.FilePullArgs{Path: "a.txt"},
	})
	if proto.CodeOf(err) != proto.CodeInternal {
		t.Fatalf("admission with a failing insert: %v", err)
	}
	if hs.wk.opened.Load() != 0 {
		t.Fatal("a leg was opened")
	}
}

// A caller cannot set the Hub's members; a refusal is recorded denied.
func TestHiddenMembersAndDenials(t *testing.T) {
	hs := newHarness(t, http.NotFoundHandler(), nil)
	_, err := hs.svc.AdmitFile(context.Background(), workers.FileCall{
		Principal: hs.who, Op: ops.OpFilePull, WorkerID: targetWorker, Session: "build",
		Args: ops.FilePullArgs{Path: "a.txt", FileLegArgs: ops.FileLegArgs{MaxBytes: 1}},
	})
	if proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("caller-set hidden member: %v", err)
	}
	if _, err := hs.svc.AdmitFile(context.Background(), workers.FileCall{
		Principal: hs.who, Op: ops.OpFilePull, WorkerID: targetWorker, Session: "build", Args: ops.FilePullArgs{Path: "../x"},
	}); proto.CodeOf(err) != proto.CodePathOutsideRoot {
		t.Fatalf("hostile path: %v", err)
	}
	hs.az.allowed.Store(false)
	if _, err := hs.svc.AdmitFile(context.Background(), workers.FileCall{
		Principal: hs.who, Op: ops.OpFilePull, WorkerID: targetWorker, Session: "build", Args: ops.FilePullArgs{Path: "a.txt"},
	}); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("refused admission: %v", err)
	}
	denied := 0
	for _, r := range hs.st.rows {
		if r.State == ops.FileStateDenied && r.Reason == string(proto.CodeForbidden) {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("denied records: %d", denied)
	}
	checkAudit(t, hs.aud.all())
}

// Policies of both ends: a Hub policy that is off refuses with files_disabled.
func TestHubPolicyOff(t *testing.T) {
	hs := newHarness(t, http.NotFoundHandler(), func(c *Config) { c.Policy = func() string { return ops.FileModeOff } })
	_, err := hs.svc.AdmitFile(context.Background(), workers.FileCall{
		Principal: hs.who, Op: ops.OpFilePush, WorkerID: targetWorker, Session: "build",
		Args: ops.FilePushArgs{Path: "a", As: "a", Kind: ops.FileKindFile, Size: 1},
	})
	if proto.CodeOf(err) != proto.CodeFilesDisabled {
		t.Fatalf("policy off: %v", err)
	}
}

// A push relays the body, forwards the request trailers after the gate, and
// ends with the target's 201 receipt.
func TestPushRelay(t *testing.T) {
	var tok string
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(proto.HeaderLegToken) != tok {
			legError(w, proto.CodeForbidden, "bad token")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, content) || r.Trailer.Get(proto.HeaderContentDigest) != digestField {
			legError(w, proto.CodeIntegrityFailed, "not verified")
			return
		}
		w.Header().Set("Content-Type", proto.ContentTypeReceipt)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ops.FileReceipt{Outcome: ops.OutcomeDelivered, Files: 1, Bytes: int64(len(body)), ManifestDigest: contentHex})
	}
	hs := newHarness(t, http.HandlerFunc(h), nil)
	adm, d := hs.admit(ops.OpFilePush, ops.FilePushArgs{Path: "a.txt", As: "in/a.txt", Kind: ops.FileKindFile, Size: 11}, fileAnswer())
	tok = targetToken(t, d)
	req, _ := http.NewRequest(http.MethodPut, hs.srv.URL+adm.LegPath, io.NopCloser(bytes.NewReader(content)))
	req.ContentLength = -1
	req.Trailer = http.Header{proto.HeaderContentDigest: {digestField}}
	req.Header.Set(proto.HeaderLegToken, adm.LegToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push: %d %s", resp.StatusCode, body)
	}
	r := waitState(t, hs.st, adm.TransferID, ops.FileStateDelivered)
	if r.Files != 1 || r.ManifestDigest != contentHex {
		t.Fatalf("record: %+v", r)
	}
	// A GET on a push leg is refused.
	checkAudit(t, hs.aud.all())
}

// A leg from another identity, or with the method of the other direction,
// is refused and the transfer stays open for its caller.
func TestLegBinding(t *testing.T) {
	var tok string
	hs := newHarness(t, servePull(&tok, nil), nil)
	adm, d := hs.admit(ops.OpFilePull, ops.FilePullArgs{Path: "a.txt"}, fileAnswer())
	tok = targetToken(t, d)
	other := hs.who
	hs.who = authz.NewSessionPrincipal(callerUser)
	if resp, _, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("leg of another identity: %v %v", resp, err)
	}
	hs.who = other
	if resp, _, err := get(t, hs.srv.URL+adm.LegPath, tok); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("leg with the target's token: %v %v", resp, err)
	}
	if resp, body, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken); err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("caller leg: %v %v", resp, err)
	}
}

// A leg that is not opened in time ends the record failed_not_sent.
func TestExpiry(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	hs := newHarness(t, http.NotFoundHandler(), func(c *Config) { c.Now = func() time.Time { return clock() } })
	adm, _ := hs.admit(ops.OpFilePull, ops.FilePullArgs{Path: "a.txt"}, fileAnswer())
	now = now.Add(2 * proto.LegOpenWindow)
	if resp, _, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken); err != nil || resp.StatusCode != http.StatusGone {
		t.Fatalf("expired leg: %v %v", resp, err)
	}
}

// After a restart every open record is unknown; a late receipt resolves it.
func TestRecoverAndLateReceipt(t *testing.T) {
	var tok string
	hs := newHarness(t, servePull(&tok, nil), nil)
	adm, _ := hs.admit(ops.OpFilePull, ops.FilePullArgs{Path: "a.txt"}, fileAnswer())
	// A new process: same store, no live transfers.
	fresh, err := New(Config{Store: hs.st, Authz: hs.az, Legs: hs.wk, Conn: func(string) []byte { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fresh.Recover(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != adm.TransferID {
		t.Fatalf("recover: %v %v", ids, err)
	}
	got, err := fresh.ReceiptFor(context.Background(), hs.who, ops.FileReceiptArgs{TransferID: adm.TransferID, LegToken: adm.LegToken,
		FileReceipt: ops.FileReceipt{Outcome: ops.OutcomeDelivered, Files: 1, Bytes: 11, ManifestDigest: contentHex}})
	if err != nil || got.State != ops.FileStateDelivered {
		t.Fatalf("late receipt: %+v %v", got, err)
	}
}

// The history: owner-scoped (the store applies the filter), a Worker
// identity has none.
func TestHistory(t *testing.T) {
	hs := newHarness(t, http.NotFoundHandler(), nil)
	adm, _ := hs.admit(ops.OpFilePull, ops.FilePullArgs{Path: "a.txt"}, fileAnswer())
	res, err := hs.svc.History(context.Background(), authz.NewSessionPrincipal(callerUser), ops.FileTransfersListArgs{})
	if err != nil || len(res.Transfers) != 1 || res.Transfers[0].TransferID != adm.TransferID {
		t.Fatalf("owner history: %+v %v", res, err)
	}
	other, err := hs.svc.History(context.Background(), authz.NewSessionPrincipal("01900000-0000-7000-8000-0000000000cc"), ops.FileTransfersListArgs{})
	if err != nil || len(other.Transfers) != 0 {
		t.Fatalf("unrelated user: %+v %v", other, err)
	}
	if _, err := hs.svc.History(context.Background(), authz.NewWorkerPrincipal(targetWorker), ops.FileTransfersListArgs{}); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("worker history: %v", err)
	}
}

func TestDetailGrammar(t *testing.T) {
	r := store.FileTransferRow{ID: "01900000-0000-7000-8000-000000000009", Direction: ops.FileDirectionPull, Kind: ops.FileKindDir,
		Files: 12, Bytes: 123456, Reason: "integrity_failed", ManifestDigest: contentHex, Path: "secret dir/x", Protected: "off"}
	d := Detail(r, ops.FileStatePartial)
	if !audit.ValidDetail(d) || strings.Contains(d, "secret") {
		t.Fatalf("detail %q", d)
	}
	want := "transfer=01900000-0000-7000-8000-000000000009 dir=pull kind=dir files=12 bytes=123456 wire=0 outcome=partial reason=integrity_failed manifest=" + contentHex + " protected=off"
	if d != want {
		t.Fatalf("detail\n got %q\nwant %q", d, want)
	}
}
