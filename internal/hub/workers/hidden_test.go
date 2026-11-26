package workers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

var (
	factsHidden = CleanFacts(transport.WorkerFacts{SessionHost: "conpty", SessionsHidden: true, SessionHostReasonCode: transport.HostReasonConPTYNoPsmux})
	// factsVisible says psmux; a ConPTY report with a false flag is still hidden.
	factsVisible = CleanFacts(transport.WorkerFacts{SessionHost: "psmux", SessionHostReasonCode: transport.HostReasonPsmuxDefault})
	factsUnknown = transport.WorkerFacts{Build: "v0.9.3", OS: "linux"}
	// factsLegacyWindows is a Windows Worker older than v0.10.0: it may host
	// ConPTY, so it counts as hidden.
	factsLegacyWindows = transport.WorkerFacts{Build: "v0.9.3", OS: "windows"}
	// factsPreV09 is a Worker older than v0.9.0 (v0.8.2): no OS, no build, no
	// session host. Its host is unknown: hidden under disabled only.
	factsPreV09 = transport.WorkerFacts{}
)

// The policy table: 3 Hub values x the user's choice x hidden true, false or
// unknown (an older Worker).
func TestHiddenSessionPolicyTable(t *testing.T) {
	for _, hub := range []string{config.HiddenSessionsUserControlled, config.HiddenSessionsAllowed, config.HiddenSessionsDisabled} {
		for _, user := range []bool{false, true} {
			for name, f := range map[string]transport.WorkerFacts{"hidden": factsHidden, "visible": factsVisible, "unknown": factsUnknown, "legacy windows": factsLegacyWindows, "pre-v0.9": factsPreV09} {
				t.Run(fmt.Sprintf("%s/user=%v/%s", hub, user, name), func(t *testing.T) {
					pol := HiddenSessionsPolicy{Hub: hub, UserAllows: user}
					wantAllowed := hub == config.HiddenSessionsAllowed || (hub == config.HiddenSessionsUserControlled && user)
					if pol.Allows() != wantAllowed {
						t.Fatalf("Allows = %v, want %v", pol.Allows(), wantAllowed)
					}
					msg := HiddenSessionRefusal("patryk-asus", f, pol)
					wantRefused := (name == "hidden" || name == "legacy windows" || (name == "pre-v0.9" && hub == config.HiddenSessionsDisabled)) && !wantAllowed
					if (msg != "") != wantRefused {
						t.Fatalf("refusal = %q, want refused=%v", msg, wantRefused)
					}
					if !wantRefused {
						return
					}
					parts := []string{"New sessions on patryk-asus would be hidden (ConPTY inside the Worker: psmux is not installed", "winget install psmux", "minion worker up --foreground"}
					switch name {
					case "legacy windows":
						parts = []string{"New sessions on patryk-asus may be hidden", "older than v0.10.0", "Update the Worker to v0.10.0 or later"}
					case "pre-v0.9":
						parts = []string{"New sessions on patryk-asus may be hidden", "session host unknown", "Update the Worker to v0.10.0 or later"}
					}
					for _, part := range parts {
						if !strings.Contains(msg, part) {
							t.Errorf("refusal %q lacks %q", msg, part)
						}
					}
					if hub == config.HiddenSessionsDisabled {
						if !strings.Contains(msg, "disabled on this Hub") || strings.Contains(msg, "Account > Sessions") {
							t.Errorf("a disabled Hub must not send the user to their settings: %q", msg)
						}
					} else if !strings.Contains(msg, "allow hidden sessions on this Worker") || !strings.Contains(msg, "in your settings (Account > Sessions)") {
						t.Errorf("refusal %q does not name the user setting", msg)
					}
				})
			}
		}
	}
}

func TestHiddenSessionRefusalFixByHost(t *testing.T) {
	pol := HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled}
	svc := CleanFacts(transport.WorkerFacts{SessionHost: "conpty", SessionsHidden: true, SessionHostReasonCode: transport.HostReasonConPTYService})
	if msg := HiddenSessionRefusal("w", svc, pol); !strings.Contains(msg, "instead of as a Windows service") {
		t.Errorf("service fix = %q", msg)
	}
	tm := CleanFacts(transport.WorkerFacts{SessionHost: "tmux", SessionsHidden: true, SessionHostReasonCode: transport.HostReasonPrivateSocket})
	if msg := HiddenSessionRefusal("", tm, pol); !strings.Contains(msg, "this Worker") || !strings.Contains(msg, "default tmux server") {
		t.Errorf("tmux fix = %q", msg)
	}
	ct := CleanFacts(transport.WorkerFacts{SessionHost: "tmux", SessionsHidden: true, SessionHostReasonCode: transport.HostReasonContainerTmux})
	if msg := HiddenSessionRefusal("w", ct, pol); !strings.Contains(msg, "sessions_visible") {
		t.Errorf("container fix = %q", msg)
	}
	// ConPTY is hidden even when a Worker says otherwise.
	lie := transport.WorkerFacts{SessionHost: "conpty", SessionsHidden: false}
	if msg := HiddenSessionRefusal("w", lie, pol); msg == "" {
		t.Error("a ConPTY report with a false hidden flag was not refused")
	}
	if got := CleanFacts(lie); !got.SessionsHidden {
		t.Error("CleanFacts kept a visible ConPTY host")
	}
}

// CleanFacts keeps only reason codes with the Hub's own fixed text: a path,
// a user name or program output in the Worker's text never reaches people.
func TestCleanFactsReplacesReasonTextsByTheFixedTexts(t *testing.T) {
	got := CleanFacts(transport.WorkerFacts{SessionHost: "tmux", SessionsHidden: true,
		SessionHostReasonCode: transport.HostReasonPrivateSocket, SessionHostReason: "private tmux socket /home/pat/s",
		SessionsListErrorCode: transport.ListFailedExec, SessionsListError: "exit status 1: /home/pat secret"})
	if got.SessionHostReason != transport.HostReasonText(transport.HostReasonPrivateSocket) || strings.Contains(got.SessionHostReason, "/home") {
		t.Errorf("reason = %q", got.SessionHostReason)
	}
	if got.SessionsListError != transport.ListErrorText(transport.ListFailedExec) || got.SessionsListErrorCode != transport.ListFailedExec {
		t.Errorf("list error = %q (%q)", got.SessionsListError, got.SessionsListErrorCode)
	}
	// No code: no text.
	got = CleanFacts(transport.WorkerFacts{SessionHost: "tmux", SessionHostReason: "pat", SessionsListError: "x"})
	if got.SessionHostReason != "" || got.SessionsListError != "" {
		t.Errorf("text without a code kept: %+v", got)
	}
}

func hiddenRig(t *testing.T, pol HiddenSessionsPolicy, polErr error, facts transport.WorkerFacts) (*rig, *[]authz.Principal) {
	t.Helper()
	var asked []authz.Principal
	r := newRig(t, func(c *RouterConfig) {
		c.HiddenSessions = func(_ context.Context, p authz.Principal) (HiddenSessionsPolicy, error) {
			asked = append(asked, p)
			return pol, polErr
		}
	})
	r.be.online[wAlicePi1.ID] = Online{Facts: facts}
	return r, &asked
}

func createReq() CallRequest {
	return CallRequest{Principal: routeAlice(), Op: ops.OpSessionCreate, WorkerID: wAlicePi1.ID,
		Args: ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}}}
}

func TestSessionCreateOnAHiddenWorkerIsRefusedBeforeAnythingIsSent(t *testing.T) {
	r, asked := hiddenRig(t, HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled}, nil, factsHidden)
	_, err := r.rt.Call(context.Background(), createReq())
	if code(err) != proto.CodeForbidden || !strings.Contains(err.Error(), "New sessions on pi-1 would be hidden") {
		t.Fatalf("err = %v, want the hidden-session refusal", err)
	}
	if r.be.openCount() != 0 {
		t.Error("a stream was opened for a refused session_create")
	}
	if len(*asked) != 1 || (*asked)[0].UserID != idAlice {
		t.Errorf("policy asked for %+v, want the caller", *asked)
	}
	// Other ops on the same Worker are not affected.
	r.script(newFakeStream(resultFrame(t, sessionsResult())))
	if _, err := r.rt.Call(context.Background(), CallRequest{Principal: routeAlice(), Op: ops.OpSessionsList, WorkerID: wAlicePi1.ID, Args: ops.SessionsListArgs{}}); err != nil {
		t.Fatalf("sessions_list on a hidden Worker: %v", err)
	}
}

func TestSessionCreateIsSentWhenAllowedVisibleOrUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		pol   HiddenSessionsPolicy
		facts transport.WorkerFacts
	}{
		"user allows":                   {HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled, UserAllows: true}, factsHidden},
		"hub allows":                    {HiddenSessionsPolicy{Hub: config.HiddenSessionsAllowed}, factsHidden},
		"visible":                       {HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}, factsVisible},
		"older Worker":                  {HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}, factsUnknown},
		"older Windows Worker, allowed": {HiddenSessionsPolicy{Hub: config.HiddenSessionsAllowed}, factsLegacyWindows},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := hiddenRig(t, tc.pol, nil, tc.facts)
			r.script(newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
			if _, err := r.rt.Call(context.Background(), createReq()); err != nil {
				t.Fatalf("session_create: %v", err)
			}
			if r.be.openCount() != 1 {
				t.Errorf("opens = %v", r.be.opens)
			}
		})
	}
}

// The race of the review: the Router looked at a visible (or offline)
// Worker, and the Worker reconnects with hidden facts before the op is
// opened. The check runs on the connection that gets the op, so nothing is
// sent.
func TestSessionCreateIsJudgedOnTheConnectionThatGetsIt(t *testing.T) {
	for name, before := range map[string]*Online{"visible before": {Facts: factsVisible}, "offline before": nil} {
		t.Run(name, func(t *testing.T) {
			r, _ := hiddenRig(t, HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}, nil, factsVisible)
			if before == nil {
				delete(r.be.online, wAlicePi1.ID)
			}
			r.be.beforeOpen = func(b *fakeBackend) { b.online[wAlicePi1.ID] = Online{Facts: factsHidden} }
			r.script(newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
			_, err := r.rt.Call(context.Background(), createReq())
			if code(err) != proto.CodeForbidden || !strings.Contains(err.Error(), "would be hidden") {
				t.Fatalf("err = %v, want the hidden-session refusal", err)
			}
			if r.be.openCount() != 0 {
				t.Errorf("the reconnected hidden Worker got the op: %v", r.be.opens)
			}
		})
	}
}

// The registry runs the check on the entry it opens the stream on: the
// replacement connection of a reconnect, not the one before.
func TestOpenOpCheckedJudgesTheSelectedConnection(t *testing.T) {
	reg := NewRegistry()
	old := newFakeSession()
	visible := info("w1", ops.OpSessionCreate)
	visible.Facts = factsVisible
	reg.Add(visible, old)
	hidden := info("w1", ops.OpSessionCreate)
	hidden.Facts = factsHidden
	repl := newFakeSession()
	reg.Add(hidden, repl)
	pol := HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}
	check := func(on Online) error {
		if msg := HiddenSessionRefusal("w1", on.Facts, pol); msg != "" {
			return proto.NewError(proto.CodeForbidden, msg)
		}
		return nil
	}
	if _, err := reg.OpenOpChecked(context.Background(), "w1", ops.OpSessionCreate, check); proto.CodeOf(err) != proto.CodeForbidden {
		t.Fatalf("err = %v, want forbidden", err)
	}
	if old.openCount()+repl.openCount() != 0 {
		t.Error("a stream was opened for a refused op")
	}
}

func TestSessionCreateFailsClosedWhenThePolicyCannotBeRead(t *testing.T) {
	r, _ := hiddenRig(t, HiddenSessionsPolicy{}, errors.New("disk on fire"), factsHidden)
	if _, err := r.rt.Call(context.Background(), createReq()); err == nil || r.be.openCount() != 0 {
		t.Fatalf("err = %v, opens = %v; want a refusal before sending", err, r.be.opens)
	}
}

func TestWorkersListCarriesTheSessionHostAndTheRefusal(t *testing.T) {
	r, _ := hiddenRig(t, HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled}, nil, factsHidden)
	r.be.online[wAlicePi2.ID] = Online{Facts: factsVisible}
	list, err := r.rt.ListWorkers(context.Background(), routeAlice(), false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ops.WorkerInfo{}
	for _, w := range list {
		got[w.ID] = w
	}
	h := got[wAlicePi1.ID]
	if h.SessionHost != "conpty" || !h.SessionsHidden || h.SessionHostReason == "" || !strings.Contains(h.NewSessionRefused, "would be hidden") {
		t.Errorf("hidden Worker = %+v", h)
	}
	v := got[wAlicePi2.ID]
	if v.SessionHost != "psmux" || v.SessionsHidden || v.NewSessionRefused != "" {
		t.Errorf("visible Worker = %+v", v)
	}
}

func TestCleanFactsKeepsAKnownSessionHostOnly(t *testing.T) {
	got := CleanFacts(transport.WorkerFacts{SessionHost: "conpty", SessionsHidden: true, SessionHostReason: "why\x1b[2J"})
	if got.SessionHost != "conpty" || !got.SessionsHidden || strings.ContainsRune(got.SessionHostReason, 0x1b) {
		t.Errorf("clean = %+v", got)
	}
	got = CleanFacts(transport.WorkerFacts{SessionHost: "screen", SessionsHidden: true, SessionHostReason: "x"})
	if got.SessionHost != "" || got.SessionsHidden || got.SessionHostReason != "" {
		t.Errorf("an unknown host was kept: %+v", got)
	}
}

func TestWorkersListSaysWhetherTheSessionListArrived(t *testing.T) {
	r := newRig(t, nil)
	r.be.online[wAlicePi1.ID] = Online{Facts: transport.WorkerFacts{SessionsListError: "psmux: server not running"}}
	r.be.online[wAlicePi2.ID] = Online{}
	r.rt.SetSessionsSynced(func(id string) bool { return id == wAlicePi2.ID })
	list, err := r.rt.ListWorkers(context.Background(), routeAlice(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		switch w.ID {
		case wAlicePi1.ID:
			if w.SessionsSynced || w.SessionsListError != "psmux: server not running" {
				t.Errorf("pi-1 = %+v", w)
			}
		case wAlicePi2.ID:
			if !w.SessionsSynced || w.SessionsListError != "" {
				t.Errorf("pi-2 = %+v", w)
			}
		}
	}
}

// Round 2: the hub_check the Hub kept for a Worker is listed while it is
// offline; a connected Worker's own hello wins.
func TestWorkersListKeepsTheHubCheckOfAnOfflineWorker(t *testing.T) {
	r := newRig(t, nil)
	for i := range r.dir.recs {
		switch r.dir.recs[i].ID {
		case wAlicePi1.ID:
			r.dir.recs[i].HubCheck = "none"
		case wAlicePi2.ID:
			r.dir.recs[i].HubCheck = "none"
		}
	}
	r.be.online[wAlicePi2.ID] = Online{Facts: transport.WorkerFacts{HubCheck: "fingerprint"}}
	list, err := r.rt.ListWorkers(context.Background(), routeAlice(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		switch w.ID {
		case wAlicePi1.ID:
			if w.Online || w.HubCheck != "none" {
				t.Errorf("offline pi-1 = %+v", w)
			}
		case wAlicePi2.ID:
			if w.HubCheck != "fingerprint" {
				t.Errorf("online pi-2 = %+v", w)
			}
		}
	}
}

// An Android managed Worker keeps the tmux wire kind and reports its sessions
// as hidden: the Hub policy applies. An opted-in normal account (user
// controlled) creates sessions; hidden_sessions=disabled refuses everybody; a
// user who did not opt in is refused with the Android text, not the advice of
// a desktop Worker.
func TestAndroidManagedWorkerFollowsTheHiddenSessionPolicy(t *testing.T) {
	f := CleanFacts(transport.WorkerFacts{SessionHost: "tmux", SessionsHidden: true, SessionHostReasonCode: transport.HostReasonAndroidManaged,
		SessionHostReason: "/data/secret/path"})
	if f.SessionHost != "tmux" || !f.SessionsHidden || f.SessionHostReason != transport.HostReasonText(transport.HostReasonAndroidManaged) {
		t.Fatalf("facts = %+v", f)
	}
	for _, c := range []struct {
		hub     string
		user    bool
		refused bool
	}{
		{config.HiddenSessionsUserControlled, true, false},
		{config.HiddenSessionsUserControlled, false, true},
		{config.HiddenSessionsAllowed, false, false},
		{config.HiddenSessionsDisabled, true, true},
	} {
		msg := HiddenSessionRefusal("tv", f, HiddenSessionsPolicy{Hub: c.hub, UserAllows: c.user})
		if (msg != "") != c.refused {
			t.Errorf("hub=%s user=%v: refusal %q, want refused=%v", c.hub, c.user, msg, c.refused)
		}
		if msg != "" && (!strings.Contains(msg, "Minion Android app") || strings.Contains(msg, "minion worker up --foreground")) {
			t.Errorf("hub=%s: message %q", c.hub, msg)
		}
	}
}
