package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// The exporter is installed after the first inventory report: the Hub asks
// again soon while it is absent, so the page never keeps the old state.
func TestInventoryRefreshAsksAgainWhileTheExporterIsAbsent(t *testing.T) {
	claude := func(exporter string) []ops.UsageHarness {
		return []ops.UsageHarness{{Name: "claude", Status: "installed", Exporter: exporter}}
	}
	if got := inventoryRefresh(claude(ops.UsageExporterAbsent)); got != time.Minute {
		t.Fatalf("absent: %v", got)
	}
	if got := inventoryRefresh(claude(ops.UsageExporterInstalled)); got != 10*time.Minute {
		t.Fatalf("installed: %v", got)
	}
	if got := inventoryRefresh(nil); got != 10*time.Minute {
		t.Fatalf("none: %v", got)
	}
}

func TestUsageWebScansRequestFreshInventoryOnWorkerWire(t *testing.T) {
	d, _ := startUsageHTTPHub(t)
	owner := d.userID("root")
	const name = "scan-worker"
	wid := d.enrol(name, owner, store.WorkerKindPrivate)
	code, body, csrf := d.hc.login("root", "usage-integration-pw-1")
	if code != http.StatusOK || csrf == "" {
		t.Fatalf("login = %d %s", code, body)
	}
	hubSide, workerSide := net.Pipe()
	t.Cleanup(func() { _ = workerSide.Close(); _ = hubSide.Close() })
	hs, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan workers.OpRequest, 16)
	go func() {
		defer close(requests)
		at := time.Now().UnixMilli()
		for {
			st, err := ws.Accept(t.Context())
			if err != nil {
				return
			}
			raw, err := st.ReadFrame()
			var request workers.OpRequest
			if err != nil || json.Unmarshal(raw, &request) != nil {
				_ = st.Close()
				return
			}
			at++
			stamp := at
			report := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: stamp, InventoryAt: &stamp, Inventory: []ops.UsageHarness{{Name: "codex", Status: "installed"}}}
			payload, _ := json.Marshal(report)
			_ = st.WriteFrame(workers.OpResult{Type: proto.TypeOpResult, Result: payload})
			_ = st.Close()
			select {
			case requests <- request:
			case <-t.Context().Done():
				return
			}
		}
	}()
	d.wsv.Registry().Add(workers.Online{WorkerID: wid, Name: name, OwnerID: owner, Kind: store.WorkerKindPrivate, Ops: []string{ops.OpWorkerUsage}}, hs)

	check := func(label string, origin proto.OriginKind, fresh bool) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case request, ok := <-requests:
				if !ok {
					t.Fatalf("Worker connection ended before %s", label)
				}
				if request.Origin.Kind != origin {
					continue
				}
				var args map[string]any
				if request.Op != ops.OpWorkerUsage || json.Unmarshal(request.Args, &args) != nil || args["worker_id"] != wid || args["inventory"] != true {
					t.Fatalf("%s sent %s %s", label, request.Op, request.Args)
				}
				if fresh && args["fresh"] != true || !fresh && args["fresh"] != nil {
					t.Errorf("%s sent %s, want fresh=%t", label, request.Args, fresh)
				}
				return
			case <-timeout:
				t.Fatalf("Worker did not receive %s", label)
			}
		}
	}
	check("periodic inventory", proto.OriginWorker, false)
	for _, path := range []string{"/usage/scan", "/harnesses/scan"} {
		code, body, response := formDo(d.hc, http.MethodPost, path, csrf, url.Values{"for_user": {owner}, "worker": {wid}, "view": {"workers"}})
		// Usage reloads the page; Harnesses answers inline with the persisted result.
		reloads := response.Header.Get("HX-Redirect") != "" || response.Header.Get("HX-Refresh") == "true"
		inline := response.Header.Get("HX-Trigger") == "harnesses-checked" && strings.Contains(body, "Checked 1 Worker.")
		if code != http.StatusOK || (path == "/usage/scan") != reloads || (path == "/harnesses/scan") != inline {
			t.Fatalf("%s = %d %s %v", path, code, body, response.Header)
		}
		check(path, proto.OriginUser, true)
	}
	if code, body, _ := d.hc.do(http.MethodGet, "/harnesses", nil, nil); code != http.StatusOK || strings.Contains(body, "Not checked yet") {
		t.Errorf("Harnesses did not show the persisted scan time: %d", code)
	}
}
