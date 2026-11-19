package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Gate-1 C: the stop fallback of a stopped-looking profile never stops a
// unit that is registered for another profile.
func TestStopFallbackChecksUnitProfile(t *testing.T) {
	mine, other := t.TempDir(), t.TempDir()
	d := baseDown()
	d.call = func(context.Context, string) (json.RawMessage, error) { return nil, client.ErrWorkerNotRunning }
	stopped := false
	d.output = func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "Environment") {
			return "A=b MINION_STATE_DIR=" + other + "\n", nil
		}
		return "active\n", nil
	}
	d.run = func(_ context.Context, _ string, args ...string) error {
		if len(args) > 1 && args[1] == "stop" {
			stopped = true
		}
		return nil
	}
	m, code := downJSON(t, d, "--state-dir", mine)
	if code != ExitStateChanged || stopped {
		t.Fatalf("other profile unit: %v %d stopped=%v", m, code, stopped)
	}
}

// A Worker that starts answering between the probes on a ConPTY host is not
// stopped without consent.
func TestStopFallbackLateWorkerNeedsConsent(t *testing.T) {
	d := baseDown()
	d.goos = "windows"
	n := 0
	sent := []string{}
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		sent = append(sent, op)
		n++
		if n == 1 {
			return nil, client.ErrWorkerNotRunning
		}
		return json.Marshal(agent.LiveStatusReply{StateDir: "/x", SessionHost: "conpty"})
	}
	m, code := downJSON(t, d)
	if code != ExitCountUnavailable {
		t.Fatalf("late worker: %v %d", m, code)
	}
	for _, op := range sent {
		if op == agent.OpLocalStop {
			t.Fatal("worker_stop was sent")
		}
	}
}

// A system unit this user may not stop is refused before the Worker takes a
// create hold (no lifecycle op).
func TestStopNeedsAdminBeforeHold(t *testing.T) {
	d := baseDown()
	d.call = func(context.Context, string) (json.RawMessage, error) {
		return json.Marshal(agent.LiveStatusReply{StateDir: "/x", Instance: "i", SessionHost: "tmux"})
	}
	called := false
	d.callArgs = func(context.Context, string, any) (json.RawMessage, error) { called = true; return nil, nil }
	d.output = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "--user" {
			return "inactive\n", nil
		}
		return "active\n", nil
	}
	m, code := downJSON(t, d)
	if code != ExitNeedsAdmin || called {
		t.Fatalf("%v %d lifecycle called=%v", m, code, called)
	}
}

func stopCmd() *cobra.Command { return &cobra.Command{Use: "down"} }

// Gate-1 C: leave's stop is bound to the instance that answered and to the
// enrollment generation: the lifecycle op carries both, a refusal is 27.
func TestStopSendsInstanceAndEpochAndRefusalIs27(t *testing.T) {
	d := baseDown()
	d.call = func(context.Context, string) (json.RawMessage, error) {
		return json.Marshal(agent.LiveStatusReply{StateDir: "/x", Instance: "i1", SessionHost: "tmux"})
	}
	var got agent.LocalLifecycleArgs
	var op string
	d.callArgs = func(_ context.Context, o string, a any) (json.RawMessage, error) {
		op, got = o, a.(agent.LocalLifecycleArgs)
		pe := proto.NewError(proto.CodeConflict, "the enrollment changed")
		pe.Details = json.RawMessage(`{"reason":"state_changed"}`)
		return nil, pe
	}
	epoch := uint64(7)
	_, err := runWorkerDownOpts(stopCmd(), d, downOptions{expectEpoch: &epoch})
	var re *resultError
	if !errors.As(err, &re) || re.Exit != ExitStateChanged {
		t.Fatalf("err = %v", err)
	}
	if got.Instance != "i1" || got.EnrollmentEpoch == nil || *got.EnrollmentEpoch != 7 ||
		!strings.Contains(op, agent.FeatureStopGuard) {
		t.Fatalf("op %q args %+v", op, got)
	}
}

// The bare worker_stop of the fallback carries the same guard, and an older
// Worker that does not know it gets the plain stop.
func TestLegacyStopCarriesGuardAndFallsBackOnOldWorker(t *testing.T) {
	status := func() json.RawMessage {
		b, _ := json.Marshal(agent.LiveStatusReply{StateDir: "/x", Instance: "i1", SessionHost: "tmux"})
		return b
	}
	stopped := false
	d := baseDown()
	d.call = func(_ context.Context, op string) (json.RawMessage, error) {
		switch {
		case op == agent.OpLocalStatus && stopped:
			return nil, client.ErrWorkerNotRunning
		case op == agent.OpLocalStatus:
			return status(), nil
		case op == agent.OpLocalStop:
			stopped = true
			return nil, nil
		}
		return nil, nil
	}
	var stopArgs agent.LocalStopArgs
	d.callArgs = func(_ context.Context, op string, a any) (json.RawMessage, error) {
		if strings.HasPrefix(op, agent.OpLocalLifecycle) {
			return nil, &proto.Error{Code: proto.CodeUnsupportedOp, Message: "old"}
		}
		stopArgs = a.(agent.LocalStopArgs)
		if !strings.Contains(op, agent.FeatureStopGuard) {
			t.Fatalf("stop op %q is not qualified", op)
		}
		return nil, &proto.Error{Code: proto.CodeUnsupportedOp, Message: "old"}
	}
	epoch := uint64(3)
	if _, err := runWorkerDownOpts(stopCmd(), d, downOptions{expectEpoch: &epoch}); err != nil {
		t.Fatalf("old Worker fallback: %v", err)
	}
	if stopArgs.Instance != "i1" || stopArgs.EnrollmentEpoch == nil || *stopArgs.EnrollmentEpoch != 3 || !stopped {
		t.Fatalf("stop args %+v stopped=%v", stopArgs, stopped)
	}
	// A Worker that refuses the guard (another instance) ends with 27 and is
	// not stopped.
	stopped = false
	d.callArgs = func(_ context.Context, op string, _ any) (json.RawMessage, error) {
		if strings.HasPrefix(op, agent.OpLocalLifecycle) {
			return nil, &proto.Error{Code: proto.CodeUnsupportedOp, Message: "old"}
		}
		return nil, &proto.Error{Code: proto.CodeConflict, Message: "the Worker restarted"}
	}
	_, err := runWorkerDownOpts(stopCmd(), d, downOptions{})
	var re *resultError
	if !errors.As(err, &re) || re.Exit != ExitStateChanged || stopped {
		t.Fatalf("err = %v stopped=%v", err, stopped)
	}
}
