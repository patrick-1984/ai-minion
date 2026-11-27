package workers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// verifiedFiles is a transfer service that gives a verified calling session
// its file authority (no acceptance rows), as internal/hub/files does.
type verifiedFiles struct{}

func (verifiedFiles) AdmitFile(context.Context, FileCall) (FileDispatch, error) {
	return FileDispatch{}, proto.NewError(proto.CodeUnsupportedOp, "no transfers in this test")
}

func (verifiedFiles) FileAuthority(_ context.Context, p authz.Principal, verified string, bound bool) (authz.Principal, error) {
	return p.WithFileAcceptances(verified, bound, nil), nil
}

// Finding (review of v0.25.0): an app_install that stood on a verified
// apps_install grant was withdrawn by the read re-check when the grant was
// revoked while the Android app installed; the call answered denied before
// its audit write, so an install the device still committed had no audit
// row. An app install is a write: its final admission decided it, it is not
// withdrawn later, and its row records what the target answered with the
// grant that decided it.
func TestVerifiedAppInstallOutlivesARevokeAndIsAudited(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	m.readRecheck = 20 * time.Millisecond
	m.auditor = audit.New(m.db)
	m.router.SetFileAdmitter(verifiedFiles{})
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	r.endNomination(t)
	g := r.verifiedGrantLevel(t, "first", authz.LevelControl)
	g.AppsInstall = true
	g, err := m.db.PutSessionGrant(ctx, g, g.Revision, store.GrantWrite{ActorID: "owner"})
	if err != nil || !g.AppsInstall {
		t.Fatalf("apps_install on the grant: %+v, %v", g, err)
	}
	m.RefreshLeases(r.source.ID)
	if r.lane.leases["first"].binding.LeaseID == "" {
		t.Fatal("the grant with apps_install gave no lease")
	}
	answer := ops.AppInstallResult{Outcome: ops.AppInstallInstalling, Path: "x.apk", Package: "org.example.app", VersionCode: 7}
	stream := &masterGateStream{fakeStream: newFakeStream(resultFrame(t, answer)), sent: make(chan struct{}), release: make(chan struct{})}
	r.backend.stream = stream
	close(r.backend.goOn)

	c := r.call(ops.OpAppInstall)
	c.Master.Arguments, _ = json.Marshal(ops.AppInstallArgs{WorkerID: r.target.ID, Path: "x.apk", AllowNew: true})
	type result struct {
		out any
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := m.runMasterCall(ctx, c)
		done <- result{out, err}
	}()
	select {
	case <-stream.sent:
	case res := <-done:
		t.Fatalf("the install ended before it was sent: %+v, %v", res.out, res.err)
	case <-ctx.Done():
		t.Fatal("the install was not sent")
	}
	// Sent to the Android Worker, which installs: the grant is revoked, and
	// several re-check intervals pass.
	if _, err := m.db.DeleteSessionGrant(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		t.Fatalf("an admitted install was withdrawn by a revoke: %+v, %v", res.out, res.err)
	case <-time.After(200 * time.Millisecond):
	}
	if stream.wasReset() {
		t.Fatal("the stream of an admitted install was reset")
	}
	close(stream.release)
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		t.Fatal("the install did not end")
	}
	if got, ok := appInstallResultOf(res.out); res.err != nil || !ok || got.Outcome != ops.AppInstallInstalling {
		t.Fatalf("install = %+v, %v", res.out, res.err)
	}
	rows, err := m.db.ListAudit(ctx, store.AuditQuery{Action: ops.OpAppInstall, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Outcome != ops.AppInstallInstalling || rows[0].GrantID != g.ID ||
		rows[0].Target != r.target.ID || rows[0].Source != r.source.ID+":prepared" {
		t.Fatalf("audit rows of the install = %+v", rows)
	}
}
