package workers

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestMasterRouteServesTheFileOps(t *testing.T) {
	w := ops.NewUUIDv7()
	tgt := `{"worker_id":"` + w + `","session_name":"build"}`
	for op, raw := range map[string]string{
		ops.OpFileList: `{"target":` + tgt + `,"path":"dist"}`,
		ops.OpFilePull: `{"target":` + tgt + `,"path":"dist/app.txt"}`,
		ops.OpFilePush: `{"target":` + tgt + `,"path":"fix.patch","as":"fix.patch","kind":"file","size":3}`,
	} {
		r, err := masterRoute(ops.NewUUIDv7(), op, json.RawMessage(raw))
		if err != nil || r.WorkerID != w || r.Session != "build" || r.Args == nil {
			t.Fatalf("%s: %+v, %v", op, r, err)
		}
		if !proto.IsMasterOperation(op) {
			t.Fatalf("%s is not a master operation", op)
		}
	}
	for op, raw := range map[string]string{
		ops.OpFilePull:    `{"target":` + tgt + `,"path":"a","surprise":1}`,
		ops.OpFileList:    `{"target":{"session_name":"build"}}`,
		ops.OpFileReceipt: `{}`,
	} {
		if _, err := masterRoute(ops.NewUUIDv7(), op, json.RawMessage(raw)); err == nil {
			t.Fatalf("%s %s was routed", op, raw)
		}
	}
}

// v0.25.0: app_install and app_install_status of a session caller are master
// operations, routed to the Worker their worker_id names.
func TestMasterRouteServesTheAppOps(t *testing.T) {
	w := ops.NewUUIDv7()
	for op, raw := range map[string]string{
		ops.OpAppInstall:       `{"worker_id":"` + w + `","path":"apps/x.apk","allow_new":true}`,
		ops.OpAppInstallStatus: `{"worker_id":"` + w + `"}`,
	} {
		r, err := masterRoute(ops.NewUUIDv7(), op, json.RawMessage(raw))
		if err != nil || r.WorkerID != w || r.Args == nil {
			t.Fatalf("%s: %+v, %v", op, r, err)
		}
		if !proto.IsMasterOperation(op) {
			t.Fatalf("%s is not a master operation", op)
		}
	}
	for op, raw := range map[string]string{
		ops.OpAppInstall:       `{"worker_id":"` + w + `","path":"../x.apk"}`,
		ops.OpAppInstallStatus: `{"worker_id":"not-a-uuid"}`,
	} {
		if _, err := masterRoute(ops.NewUUIDv7(), op, json.RawMessage(raw)); err == nil {
			t.Fatalf("%s %s was routed", op, raw)
		}
	}
}

// The audit event of an app install names the apps_install grants that
// decided it, else the acceptance row, as the transfer record of a file op.
func TestAppAuditIDs(t *testing.T) {
	for _, c := range []struct {
		b    authz.FileBasis
		want []string
	}{
		{authz.FileBasis{GrantIDs: []string{"g1", "g2"}, AcceptanceID: "w/a"}, []string{"g1", "g2"}},
		{authz.FileBasis{AcceptanceID: "w/a"}, []string{"w/a"}},
		{authz.FileBasis{Standing: true}, nil},
		{authz.FileBasis{}, nil},
	} {
		if got := appAuditIDs(c.b); !slices.Equal(got, c.want) {
			t.Errorf("appAuditIDs(%+v) = %v, want %v", c.b, got, c.want)
		}
	}
}
