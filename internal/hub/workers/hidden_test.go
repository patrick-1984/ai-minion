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
	factsHidden  = transport.WorkerFacts{SessionHost: "conpty", SessionsHidden: true, SessionHostReason: "ConPTY inside the Worker: psmux is not installed (winget install psmux)"}
	factsVisible = transport.WorkerFacts{SessionHost: "psmux", SessionHostReason: "the default psmux namespace of pat (psmux ls shows them)"}
	factsUnknown = transport.WorkerFacts{Build: "v0.9.3"}
)

// The policy table: 3 Hub values x the user's choice x hidden true, false or
// unknown (an older Worker).
func TestHiddenSessionPolicyTable(t *testing.T) {
	for _, hub := range []string{config.HiddenSessionsUserControlled, config.HiddenSessionsAllowed, config.HiddenSessionsDisabled} {
		for _, user := range []bool{false, true} {
			for name, f := range map[string]transport.WorkerFacts{"hidden": factsHidden, "visible": factsVisible, "unknown": factsUnknown} {
				t.Run(fmt.Sprintf("%s/user=%v/%s", hub, user, name), func(t *testing.T) {
					pol := HiddenSessionsPolicy{Hub: hub, UserAllows: user}
					wantAllowed := hub == config.HiddenSessionsAllowed || (hub == config.HiddenSessionsUserControlled && user)
					if pol.Allows() != wantAllowed {
						t.Fatalf("Allows = %v, want %v", pol.Allows(), wantAllowed)
					}
					msg := HiddenSessionRefusal("patryk-asus", f, pol)
					wantRefused := name == "hidden" && !wantAllowed
					if (msg != "") != wantRefused {
						t.Fatalf("refusal = %q, want refused=%v", msg, wantRefused)
					}
					if !wantRefused {
						return
					}
					for _, part := range []string{"New sessions on patryk-asus would be hidden (ConPTY inside the Worker: psmux is not installed", "winget install psmux", "minion worker up --foreground"} {
						if !strings.Contains(msg, part) {
							t.Errorf("refusal %q lacks %q", msg, part)
						}
					}
					if hub == config.HiddenSessionsDisabled {
						if !strings.Contains(msg, "disabled on this Hub") || strings.Contains(msg, "Account > Sessions") {
							t.Errorf("a disabled Hub must not send the user to their settings: %q", msg)
						}
					} else if !strings.Contains(msg, "allow hidden sessions in your settings (Account > Sessions)") {
						t.Errorf("refusal %q does not name the user setting", msg)
					}
				})
			}
		}
	}
}

func TestHiddenSessionRefusalFixByHost(t *testing.T) {
	pol := HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled}
	svc := transport.WorkerFacts{SessionHost: "conpty", SessionsHidden: true, SessionHostReason: "ConPTY inside the Worker: the Worker runs as a Windows service (LocalSystem)"}
	if msg := HiddenSessionRefusal("w", svc, pol); !strings.Contains(msg, "instead of as a Windows service") {
		t.Errorf("service fix = %q", msg)
	}
	tm := transport.WorkerFacts{SessionHost: "tmux", SessionsHidden: true, SessionHostReason: "private tmux socket /x/y"}
	if msg := HiddenSessionRefusal("", tm, pol); !strings.Contains(msg, "this Worker") || !strings.Contains(msg, "default tmux server") {
		t.Errorf("tmux fix = %q", msg)
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
		"user allows":  {HiddenSessionsPolicy{Hub: config.HiddenSessionsUserControlled, UserAllows: true}, factsHidden},
		"hub allows":   {HiddenSessionsPolicy{Hub: config.HiddenSessionsAllowed}, factsHidden},
		"visible":      {HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}, factsVisible},
		"older Worker": {HiddenSessionsPolicy{Hub: config.HiddenSessionsDisabled}, factsUnknown},
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
