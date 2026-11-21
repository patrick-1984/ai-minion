package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// joinCommittedState is the state a join leaves when it is killed between the
// enrollment commit and the save of the Hub address into the config file: the
// enrollment (with its Hub address and the checked fingerprint) is stored, the
// config names no Hub.
func joinCommittedState(t *testing.T, hub *identity.Key, url string) func(string, *store.Store) {
	return func(dir string, st *store.Store) {
		e := NewEnrollments(st, dir)
		if _, err := e.LoadOrCreateKey(); err != nil {
			t.Fatal(err)
		}
		if err := e.Commit(context.Background(), Enrollment{
			Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w-1", HubURL: url,
			HubCheck: store.HubCheckInvite, HubCheckAnchor: string(hub.Fingerprint()),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoverHubRequiresAnExplicitPendingJoinForTheSameHub(t *testing.T) {
	hub, _ := identity.GenerateKey()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	e := NewEnrollments(st, dir)
	file, err := config.LoadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	res := &config.Resolver{Role: config.ScopeWorker, File: file, LookupEnv: func(string) (string, bool) { return "", false }}
	url := "https://hub.example:8443"
	file.SetPendingJoin(url, hub.Fingerprint().String())
	if _, ok, err := e.RecoverHub(context.Background(), res); ok || err != nil {
		t.Fatal("recovered a Hub without an enrollment")
	}
	joinCommittedState(t, hub, url)(dir, st)
	rec, ok, err := e.RecoverHub(context.Background(), res)
	if err != nil || !ok || rec.URL != url || rec.Fingerprint != hub.Fingerprint().String() {
		t.Fatalf("recovery = %+v, %v, %v", rec, ok, err)
	}
	file.Set(config.KeyServer, "")
	if _, ok, err := e.RecoverHub(context.Background(), res); ok || err != nil {
		t.Fatal("explicit Solo did not suppress recovery")
	}
	file.Delete(config.KeyServer)
	file.SetPendingJoin("https://other.example", hub.Fingerprint().String())
	if _, ok, err := e.RecoverHub(context.Background(), res); ok || err != nil {
		t.Fatal("recovered the wrong Hub")
	}
	file.ClearPendingJoin()
	if _, ok, err := e.RecoverHub(context.Background(), res); ok || err != nil {
		t.Fatal("recovered an old enrollment without a marker")
	}
}

func pendingJoinState(t *testing.T, hub *identity.Key, url string) func(string, *store.Store) {
	t.Helper()
	return func(dir string, st *store.Store) {
		joinCommittedState(t, hub, url)(dir, st)
		file, err := config.LoadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		file.SetPendingJoin(url, hub.Fingerprint().String())
		if err := file.Save(); err != nil {
			t.Fatal(err)
		}
	}
}

// A credential-free start after a killed join connects to the Hub of the
// enrollment instead of staying in Solo mode, and a configured address wins.
func TestConnectorRecoversTheHubOfAnInterruptedJoin(t *testing.T) {
	hub, _ := identity.GenerateKey()
	c, err := startConnectorRuntime(t, nil, pendingJoinState(t, hub, "https://hub.example:8443"))
	if err != nil {
		t.Fatal(err)
	}
	c.dialer.script(func(ConnectParams) (*Attempt, error) { return nil, errors.New("scripted: unreachable") })
	eventually(t, func() bool { return c.dialer.count() > 0 })
	if got := c.dialer.call(0).HubURL; got != "https://hub.example:8443" {
		t.Errorf("dialed %q", got)
	}
	if !strings.Contains(c.logs.String(), "recovered the Hub address") {
		t.Errorf("logs = %s", c.logs.String())
	}
	if strings.Contains(c.logs.String(), "runs alone (Solo)") {
		t.Error("the Worker stayed in Solo mode")
	}
}

func TestConnectorKeepsAConfiguredHubOverTheRecoveredOne(t *testing.T) {
	hub, _ := identity.GenerateKey()
	c, err := startConnectorRuntime(t, map[string]string{"MINION_SERVER": "https://configured.example"}, pendingJoinState(t, hub, "https://hub.example:8443"))
	if err != nil {
		t.Fatal(err)
	}
	c.dialer.script(func(ConnectParams) (*Attempt, error) { return nil, errors.New("scripted: unreachable") })
	eventually(t, func() bool { return c.dialer.count() > 0 })
	if got := c.dialer.call(0).HubURL; got != "https://configured.example" {
		t.Errorf("dialed %q", got)
	}
}
