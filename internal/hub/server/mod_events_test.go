package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

func TestEventsModuleIsRegisteredAfterAPIRouteAndWorkers(t *testing.T) {
	names := DefaultRegistry.Names()
	idx := func(n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	if idx("events") < 0 || idx("events") < idx("api") || idx("events") < idx("workers") || idx("events") < idx("route") {
		t.Errorf("modules = %v", names)
	}
}

func TestEventsModuleNamesMissingServices(t *testing.T) {
	r := NewRegistry()
	registerEventsModule(r)
	err := NewKernel(r).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"events"`) {
		t.Errorf("err = %v", err)
	}
}

// eventsHub is a composed Hub with the events module, one admin and one
// enrolled Worker record.
type eventsHub struct {
	h       *running
	hc      *hubClient
	st      *store.Store
	wsv     *workers.Service
	admin   store.UserRow
	asAdmin map[string]string
	wid     string
}

func newEventsHub(t *testing.T) *eventsHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "events-integration-pw-1")
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	registerWorkersModule(r)
	registerAPI(r, ops.Default)
	registerRouteModule(r)
	registerEventsModule(r)
	var (
		st  *store.Store
		mgr *session.Manager
		wsv *workers.Service
	)
	if err := r.Register("grab", 60, func(_ context.Context, env *Env) error {
		var err error
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		wsv, err = Require[*workers.Service](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: t.TempDir()}, r)
	hc := newHubClient(t, h, "https")

	admin, err := st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	is, err := mgr.Create(context.Background(), admin, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	asAdmin := asSession(is.Token, is.CSRFToken)
	key, _ := identity.GenerateKey()
	wid := store.NewID()
	if err := st.InsertWorker(context.Background(), nil, store.WorkerRow{
		ID: wid, Pub: key.Public(), Name: "lab", OwnerID: admin.ID, Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	return &eventsHub{h: h, hc: hc, st: st, wsv: wsv, admin: admin, asAdmin: asAdmin, wid: wid}
}

// TestHubServesSessionsAndEventsFromAWorkerEventStream composes the Hub from
// the registry and runs the whole path over HTTP: a Worker session added to
// the registry opens its ev stream, the aggregated list answers from the
// ingested state, and an SSE client receives a state_changed.
func TestHubServesSessionsAndEventsFromAWorkerEventStream(t *testing.T) {
	eh := newEventsHub(t)
	h, hc, st, wsv, admin, asAdmin, wid := eh.h, eh.hc, eh.st, eh.wsv, eh.admin, eh.asAdmin, eh.wid
	_, _ = h, st
	// Anonymous callers are refused in the API error shape.
	for _, p := range []string{api.SessionsPath, api.EventsPath} {
		code, body, _ := hc.do("GET", p, nil, nil)
		if code != 401 || !strings.Contains(body, `"code":"unauthenticated"`) {
			t.Fatalf("%s anonymous = %d %s", p, code, body)
		}
	}
	code, body, _ := hc.do("GET", api.SessionsPath, nil, asAdmin)
	if code != 200 || strings.TrimSpace(body) != `{"sessions":[]}` {
		t.Fatalf("empty list = %d %s", code, body)
	}

	// Open the SSE feed first, then bring the Worker online.
	req, _ := http.NewRequest("GET", hc.base+api.EventsPath, nil)
	for k, v := range asAdmin {
		req.Header.Set(k, v)
	}
	stream := &http.Client{Transport: hc.c.Transport}
	resp, err := stream.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events = %d %v", resp.StatusCode, resp.Header)
	}
	events := make(chan [2]string, 16)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		var cur [2]string
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "":
				if cur[0] != "" {
					events <- cur
				}
				cur = [2]string{}
			case strings.HasPrefix(line, "event: "):
				cur[0] = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur[1] = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	expect := func(name string) [2]string {
		t.Helper()
		select {
		case e, ok := <-events:
			if !ok || e[0] != name {
				t.Fatalf("got %v (open %v), want %s", e, ok, name)
			}
			return e
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s event", name)
		}
		return [2]string{}
	}
	expect("snapshot")

	hubSide, workerSide := net.Pipe()
	hs, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	wsv.Registry().Add(workers.Online{WorkerID: wid, Name: "lab", OwnerID: admin.ID, Kind: "private", Ops: []string{ops.OpSessionsList}}, hs)
	expect("worker_online")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev, err := ws.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": []map[string]any{
		{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "build"}}}); err != nil {
		t.Fatal(err)
	}
	expect("worker_resynced")
	code, body, _ = hc.do("GET", api.SessionsPath, nil, asAdmin)
	var list api.SessionsResult
	_ = json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list.Sessions) != 1 || list.Sessions[0].Worker != "lab" || list.Sessions[0].Owner != "root" ||
		list.Sessions[0].Session != "minion-1" || list.Sessions[0].Task != "build" || !list.Sessions[0].Online {
		t.Fatalf("list = %d %s", code, body)
	}
	if err := ev.WriteFrame(map[string]any{"type": proto.TypeEvent, "seq": 1, "event": "state_changed", "session": "minion-1",
		"kind": "agent:claude", "state": "busy", "task": "build"}); err != nil {
		t.Fatal(err)
	}
	var got workers.Event
	if err := json.Unmarshal([]byte(expect("state_changed")[1]), &got); err != nil {
		t.Fatal(err)
	}
	if got.WorkerID != wid || got.Session != "minion-1" || got.State != "busy" {
		t.Fatalf("event = %+v", got)
	}
}

// TestHubShutdownEndsOpenEventFeedsBeforeTheHTTPShutdown stops a composed Hub
// that has an idle SSE client. The feed must end with a "reset" event at once:
// the HTTP server's graceful shutdown waits for open handlers, so a feed that
// ended only when the kernel stopped would hold Run for the whole shutdown
// timeout and be cut without the event.
func TestHubShutdownEndsOpenEventFeedsBeforeTheHTTPShutdown(t *testing.T) {
	eh := newEventsHub(t)
	req, _ := http.NewRequest("GET", eh.hc.base+api.EventsPath, nil)
	for k, v := range eh.asAdmin {
		req.Header.Set(k, v)
	}
	stream := &http.Client{Transport: eh.hc.c.Transport}
	resp, err := stream.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events = %d", resp.StatusCode)
	}
	events := make(chan [2]string, 16)
	go func() {
		defer close(events)
		sc := bufio.NewScanner(resp.Body)
		var cur [2]string
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "":
				if cur[0] != "" {
					events <- cur
				}
				cur = [2]string{}
			case strings.HasPrefix(line, "event: "):
				cur[0] = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur[1] = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case e := <-events:
		if e[0] != "snapshot" {
			t.Fatalf("first event = %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no snapshot")
	}

	began := time.Now()
	eh.h.cancel()
	var reset [2]string
	select {
	case e, ok := <-events:
		if !ok {
			t.Fatal("the feed closed without a reset event")
		}
		reset = e
	case <-time.After(5 * time.Second):
		t.Fatal("no reset event after the Hub began to stop")
	}
	if reset[0] != "reset" || !strings.Contains(reset[1], "shutdown") {
		t.Fatalf("got %v, want a reset with reason shutdown", reset)
	}
	eh.h.stop(t)
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("Run returned after %v, want well under the %v shutdown timeout", d, DefaultShutdownTimeout)
	}
	if logs := eh.h.logs.String(); strings.Contains(logs, "graceful shutdown timed out") {
		t.Fatalf("the HTTP shutdown timed out:\n%s", logs)
	}
}
