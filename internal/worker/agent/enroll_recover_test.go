package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

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

func TestRecoverHubFromACommittedEnrollment(t *testing.T) {
	hub, _ := identity.GenerateKey()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	e := NewEnrollments(st, dir)
	if _, _, err := e.RecoverHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.RecoverHub(context.Background()); ok {
		t.Fatal("recovered a Hub without an enrollment")
	}
	joinCommittedState(t, hub, "https://hub.example:8443")(dir, st)
	rec, ok, err := e.RecoverHub(context.Background())
	if err != nil || !ok {
		t.Fatalf("%+v %v %v", rec, ok, err)
	}
	if rec.URL != "https://hub.example:8443" || rec.Fingerprint != string(hub.Fingerprint()) {
		t.Errorf("rec = %+v", rec)
	}
}

func TestRecoverHubIgnoresAnUnusableAddressAndAnUncheckedPin(t *testing.T) {
	hub, _ := identity.GenerateKey()
	for name, enr := range map[string]Enrollment{
		"no address":  {Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w"},
		"bad address": {Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w", HubURL: "ftp://x"},
	} {
		dir := t.TempDir()
		st, _ := store.Open(dir)
		e := NewEnrollments(st, dir)
		if _, err := e.LoadOrCreateKey(); err != nil {
			t.Fatal(err)
		}
		if err := e.Commit(context.Background(), enr); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := e.RecoverHub(context.Background()); ok || err != nil {
			t.Errorf("%s: ok=%v err=%v", name, ok, err)
		}
		_ = st.Close()
	}
	// An enrollment checked by nothing recovers the address, not a fingerprint.
	dir := t.TempDir()
	st, _ := store.Open(dir)
	defer func() { _ = st.Close() }()
	e := NewEnrollments(st, dir)
	if _, err := e.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	if err := e.Commit(context.Background(), Enrollment{Pin: identity.Pin{Pub: hub.Public()}, WorkerID: "w", HubURL: "https://h.example"}); err != nil {
		t.Fatal(err)
	}
	rec, ok, _ := e.RecoverHub(context.Background())
	if !ok || rec.Fingerprint != "" {
		t.Errorf("rec = %+v ok=%v", rec, ok)
	}
}

// A credential-free start after a killed join connects to the Hub of the
// enrollment instead of staying in Solo mode, and a configured address wins.
func TestConnectorRecoversTheHubOfAnInterruptedJoin(t *testing.T) {
	hub, _ := identity.GenerateKey()
	c, err := startConnectorRuntime(t, nil, joinCommittedState(t, hub, "https://hub.example:8443"))
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
	c, err := startConnectorRuntime(t, map[string]string{"MINION_SERVER": "https://configured.example"}, joinCommittedState(t, hub, "https://hub.example:8443"))
	if err != nil {
		t.Fatal(err)
	}
	c.dialer.script(func(ConnectParams) (*Attempt, error) { return nil, errors.New("scripted: unreachable") })
	eventually(t, func() bool { return c.dialer.count() > 0 })
	if got := c.dialer.call(0).HubURL; got != "https://configured.example" {
		t.Errorf("dialed %q", got)
	}
}
