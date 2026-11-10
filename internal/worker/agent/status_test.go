package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

type fakeEnrollment struct {
	enr Enrollment
	ok  bool
	err error
}

func (f fakeEnrollment) Load(context.Context) (Enrollment, bool, error) { return f.enr, f.ok, f.err }

type fakeKeys struct {
	key *identity.Key
	err error
}

func (f fakeKeys) LoadKey() (*identity.Key, error) { return f.key, f.err }

type fakeShare struct {
	sel store.ShareSelection
	err error
}

func (f fakeShare) LoadShareContext(context.Context) (store.ShareSelection, error) {
	return f.sel, f.err
}

type fakeSessions struct {
	n   int
	err error
}

func (f fakeSessions) ListSessions(context.Context) ([]term.Session, error) {
	return make([]term.Session, f.n), f.err
}

type fakeLive struct {
	ls  LiveStatus
	err error
}

func (f fakeLive) LiveStatus(context.Context) (LiveStatus, error) { return f.ls, f.err }

func seedKey(t *testing.T, b byte) *identity.Key {
	t.Helper()
	k, err := identity.KeyFromSeed(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var statusTime = time.Date(2026, 11, 10, 12, 30, 0, 0, time.UTC)

// pairedInputs is a Hub Worker that is paired, shares selected sessions and
// has a running connector.
func pairedInputs(t *testing.T) StatusInputs {
	hub := seedKey(t, 1)
	return StatusInputs{
		Version:     "v0.1.0",
		Name:        "lab-pi",
		HubURL:      "https://hub.example:8443",
		TmuxSocket:  "/run/user/1000/tmux-1000/default",
		PreviewOnly: false,
		Enrollment: fakeEnrollment{ok: true, enr: Enrollment{
			Pin: identity.Pin{Pub: hub.Public(), Seq: 2}, WorkerID: "w1", Owner: "alice",
		}},
		Keys:     fakeKeys{key: seedKey(t, 2)},
		Share:    fakeShare{sel: store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"build", "docs"}, Configured: true}},
		Sessions: fakeSessions{n: 3},
		Live: fakeLive{ls: LiveStatus{PairingKnown: true, Connector: Status{
			State: ConnConnected, ConnectedSince: statusTime,
		}}},
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file (run with -update to rewrite it)\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func render(t *testing.T, r StatusReport) (text, js []byte) {
	t.Helper()
	var tb, jb bytes.Buffer
	if err := r.WriteText(&tb); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteJSON(&jb); err != nil {
		t.Fatal(err)
	}
	return tb.Bytes(), jb.Bytes()
}

func TestStatusGoldenPairedHubWorker(t *testing.T) {
	text, js := render(t, BuildStatus(context.Background(), pairedInputs(t)))
	golden(t, "status_paired.txt.golden", text)
	golden(t, "status_paired.json.golden", js)
}

func TestStatusGoldenUnpairedSoloWorker(t *testing.T) {
	r := BuildStatus(context.Background(), StatusInputs{
		Version: "dev", Name: "laptop", TmuxSocket: "/tmp/tmux-1000/default",
		Enrollment: fakeEnrollment{}, Keys: fakeKeys{err: ErrNoWorkerKey},
		Share: fakeShare{sel: store.ShareSelection{Mode: store.ShareLater, Sessions: []string{}}},
	})
	text, js := render(t, r)
	golden(t, "status_solo.txt.golden", text)
	golden(t, "status_solo.json.golden", js)
}

func TestStatusGoldenWaitingForPairing(t *testing.T) {
	in := pairedInputs(t)
	in.Enrollment = fakeEnrollment{}
	in.Share = fakeShare{sel: store.ShareSelection{Mode: store.ShareAll, Configured: true, Sessions: []string{}}}
	in.PreviewOnly = true
	in.Live = fakeLive{ls: LiveStatus{
		PendingPairingCode: "MN-ABCD-EFGH",
		Connector: Status{
			State: ConnBackoff, LastError: "dial: connection refused", LastErrorAt: statusTime,
		},
	}}
	text, js := render(t, BuildStatus(context.Background(), in))
	golden(t, "status_pairing.txt.golden", text)
	golden(t, "status_pairing.json.golden", js)
}

func TestStatusReportsEveryRequiredFact(t *testing.T) {
	r := BuildStatus(context.Background(), pairedInputs(t))
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Version:", "v0.1.0", "Mode:", "Hub\n", "https://hub.example:8443", "connected (since 2026-11-10T12:30:00Z)",
		"Last error:", "Hub fingerprint:", "Worker fingerprint:", "paired to alice", "Pairing code:",
		"select (build, docs)", "Preview only:", "/run/user/1000/tmux-1000/default", "Sessions:",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, b.String())
		}
	}
	if r.HubFingerprint != fingerprintOf(t, seedKey(t, 1)) || len(r.HubFingerprint) != identity.FingerprintLen {
		t.Errorf("hub fingerprint = %q", r.HubFingerprint)
	}
	if r.WorkerFingerprint != fingerprintOf(t, seedKey(t, 2)) {
		t.Errorf("worker fingerprint = %q", r.WorkerFingerprint)
	}
}

func fingerprintOf(t *testing.T, k *identity.Key) string {
	t.Helper()
	return k.Fingerprint().String()
}

func TestStatusJSONHasTheSameFactsAsText(t *testing.T) {
	r := BuildStatus(context.Background(), pairedInputs(t))
	var b bytes.Buffer
	if err := r.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"version", "mode", "hub_url", "connection", "hub_fingerprint", "worker_fingerprint", "paired",
		"owner", "pending_pairing_code", "share", "preview_only", "tmux_socket", "sessions",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("JSON lacks %q: %s", key, b.String())
		}
	}
	conn := m["connection"].(map[string]any)
	if conn["state"] != "connected" || conn["live"] != true {
		t.Errorf("connection = %v", conn)
	}
}

func TestStatusModeFollowsTheHubURL(t *testing.T) {
	solo := BuildStatus(context.Background(), StatusInputs{})
	if solo.Mode != ModeSolo || solo.Connection.State != ConnNone || solo.Paired {
		t.Fatalf("solo = %+v", solo)
	}
	hub := BuildStatus(context.Background(), StatusInputs{HubURL: "https://h"})
	if hub.Mode != ModeHub {
		t.Fatalf("hub = %+v", hub)
	}
}

func TestStatusWithoutARunningWorkerSaysUnknown(t *testing.T) {
	for _, live := range []LiveSource{nil, fakeLive{err: ErrNoLiveWorker}} {
		in := pairedInputs(t)
		in.Live = live
		r := BuildStatus(context.Background(), in)
		if r.Connection.State != ConnUnknown || r.Connection.Live || r.Connection.Reason == "" {
			t.Fatalf("connection = %+v", r.Connection)
		}
		if len(r.Problems) != 0 {
			t.Fatalf("a missing Worker is not a problem: %v", r.Problems)
		}
		// The facts that live in the state directory are still shown.
		if r.Owner != "alice" || r.HubFingerprint == "" {
			t.Fatalf("report = %+v", r)
		}
	}
}

// Nothing was asked of a Worker, so the text claims nothing about it: it does
// not tell the owner to start a Worker that may be running, and it does not
// say "none" for a last error that is only unknown.
func TestStatusTextClaimsNothingAboutAWorkerThatWasNotAsked(t *testing.T) {
	in := pairedInputs(t)
	in.Live = nil
	r := BuildStatus(context.Background(), in)
	text, _ := render(t, r)
	for _, want := range []string{
		"Connection:", "unknown (the connection state is known only to the running Worker and is not available here)",
		"Last error:          unknown",
	} {
		if !strings.Contains(string(text), want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"start it", "minion worker run", "Last error:          none"} {
		if strings.Contains(string(text), bad) {
			t.Errorf("text claims %q:\n%s", bad, text)
		}
	}

	// A live Worker without an error says none; Solo has no connection to fail.
	in.Live = fakeLive{ls: LiveStatus{Connector: Status{State: ConnConnected}}}
	live, _ := render(t, BuildStatus(context.Background(), in))
	if !strings.Contains(string(live), "Last error:          none") {
		t.Errorf("live text:\n%s", live)
	}
	solo, _ := render(t, BuildStatus(context.Background(), StatusInputs{}))
	if !strings.Contains(string(solo), "Last error:          none") {
		t.Errorf("solo text:\n%s", solo)
	}
}

func TestStatusUnpairedAndOwnerWording(t *testing.T) {
	in := pairedInputs(t)
	in.Enrollment = fakeEnrollment{}
	r := BuildStatus(context.Background(), in)
	if r.Paired || r.HubFingerprint != "" || r.ownerText() != "not paired" {
		t.Fatalf("unpaired = %+v", r)
	}

	in.Enrollment = fakeEnrollment{ok: true, enr: Enrollment{Pin: identity.Pin{Pub: seedKey(t, 1).Public()}}}
	in.Live = nil
	r = BuildStatus(context.Background(), in)
	if !r.Paired || r.Owner != "" || !strings.HasPrefix(r.ownerText(), "paired (") {
		t.Fatalf("paired without owner = %+v / %q", r, r.ownerText())
	}
}

func TestStatusPendingPairingCodeComesFromTheRunningWorker(t *testing.T) {
	in := pairedInputs(t)
	in.Live = fakeLive{ls: LiveStatus{PendingPairingCode: "MN-ABCD-EFGH"}}
	r := BuildStatus(context.Background(), in)
	if r.PendingPairingCode != "MN-ABCD-EFGH" {
		t.Fatalf("code = %q", r.PendingPairingCode)
	}
	var b bytes.Buffer
	_ = r.WriteText(&b)
	if !strings.Contains(b.String(), "MN-ABCD-EFGH") {
		t.Fatalf("text = %s", b.String())
	}
}

func TestStatusPreviewOnlyIsLocalOrHub(t *testing.T) {
	in := pairedInputs(t)
	cases := []struct {
		name         string
		local, hub   bool
		want         bool
		wantHub      bool
		wantTextPart string
	}{
		{"neither", false, false, false, false, "no"},
		{"local", true, false, true, false, "yes"},
		{"hub", false, true, true, true, "the Hub restricts"},
		{"both", true, true, true, true, "the Hub restricts"},
	}
	for _, c := range cases {
		in.PreviewOnly = c.local
		in.Live = fakeLive{ls: LiveStatus{HubPreviewOnly: c.hub}}
		r := BuildStatus(context.Background(), in)
		if r.PreviewOnly != c.want || r.PreviewOnlyHub != c.wantHub || !strings.Contains(r.previewText(), c.wantTextPart) {
			t.Errorf("%s: %+v / %q", c.name, r, r.previewText())
		}
	}
}

func TestStatusSurvivesFailingSources(t *testing.T) {
	boom := errors.New("boom")
	r := BuildStatus(context.Background(), StatusInputs{
		Version: "v1", HubURL: "https://h",
		Enrollment: fakeEnrollment{err: boom},
		Keys:       fakeKeys{err: boom},
		Share:      fakeShare{err: boom},
		Sessions:   fakeSessions{err: boom},
		Live:       fakeLive{err: boom},
	})
	if len(r.Problems) != 5 {
		t.Fatalf("problems = %v", r.Problems)
	}
	if r.Sessions != nil || r.sessionsText() != "unknown" {
		t.Fatalf("sessions = %v", r.Sessions)
	}
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if strings.Count(b.String(), "Problem:") != 5 {
		t.Fatalf("text = %s", b.String())
	}
}

func TestStatusNeverPrintsKeyMaterial(t *testing.T) {
	in := pairedInputs(t)
	k := seedKey(t, 2)
	r := BuildStatus(context.Background(), in)
	text, js := render(t, r)
	for _, secret := range []string{string(k.Public()), string(bytes.Repeat([]byte{2}, 32))} {
		if bytes.Contains(text, []byte(secret)) || bytes.Contains(js, []byte(secret)) {
			t.Fatal("key material leaked into the status output")
		}
	}
}

func TestLiveStatusSourceReadsConnectorPolicyAndPairing(t *testing.T) {
	p := NewPolicy(nil)
	p.SetHubPreviewOnly(true)
	got, err := LiveStatusSource{
		Policy:  p,
		Pairing: func() (string, bool) { return "MN-1111-2222", true },
	}.LiveStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.HubPreviewOnly || got.PendingPairingCode != "MN-1111-2222" {
		t.Fatalf("live = %+v", got)
	}
	empty, err := LiveStatusSource{}.LiveStatus(context.Background())
	if err != nil || empty.HubPreviewOnly || empty.PendingPairingCode != "" {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
}
