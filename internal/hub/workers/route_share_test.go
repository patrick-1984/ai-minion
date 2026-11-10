package workers

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func shareSetReq(mode string) CallRequest {
	return CallRequest{
		Principal: routeAlice(), Op: ops.OpShareSet, WorkerID: wAlicePi1.ID,
		Args: &ops.ShareSetArgs{WorkerID: wAlicePi1.ID, Mode: mode},
	}
}

// A state change that is not a Write op (share_set) whose request went out and
// whose answer did not come is told apart from every failure that happened
// before anything could change: the first is an OutcomeUnknownError (on the wire
// connection_lost), the others are the plain errors they always were. The caller
// uses the difference to keep what the change would have protected.
func TestAStateChangeThatEndsAfterTheRequestWentOutHasAnUnknownOutcome(t *testing.T) {
	t.Run("lost-after-the-request", func(t *testing.T) {
		r := newRig(t, nil)
		r.be.online[wAlicePi1.ID] = Online{WorkerID: wAlicePi1.ID, Ops: []string{ops.OpShareSet}}
		r.script(newFakeStream(readItem{err: io.ErrUnexpectedEOF}))
		_, err := r.rt.Call(context.Background(), shareSetReq("none"))
		var unknown *OutcomeUnknownError
		if !errors.As(err, &unknown) || proto.CodeOf(err) != proto.CodeConnectionLost {
			t.Fatalf("err = %#v (code %q), want an OutcomeUnknownError with connection_lost", err, proto.CodeOf(err))
		}
	})
	t.Run("answer-that-cannot-be-read", func(t *testing.T) {
		r := newRig(t, nil)
		r.be.online[wAlicePi1.ID] = Online{WorkerID: wAlicePi1.ID, Ops: []string{ops.OpShareSet}}
		r.script(newFakeStream(resultFrame(t, "not a share result")))
		_, err := r.rt.Call(context.Background(), shareSetReq("none"))
		var unknown *OutcomeUnknownError
		if !errors.As(err, &unknown) {
			t.Fatalf("err = %#v, want an OutcomeUnknownError: the request was answered but the answer says nothing", err)
		}
	})
	t.Run("refused-by-the-worker", func(t *testing.T) {
		r := newRig(t, nil)
		r.be.online[wAlicePi1.ID] = Online{WorkerID: wAlicePi1.ID, Ops: []string{ops.OpShareSet}}
		r.script(newFakeStream(errorFrame(t, proto.CodeForbidden)))
		_, err := r.rt.Call(context.Background(), shareSetReq("none"))
		var unknown *OutcomeUnknownError
		if errors.As(err, &unknown) || proto.CodeOf(err) != proto.CodeForbidden {
			t.Fatalf("err = %#v: a refusal of the Worker changed nothing and is a plain error", err)
		}
	})
	t.Run("not-reached", func(t *testing.T) {
		r := newRig(t, nil)
		r.be.openErr = errors.New("no connection")
		_, err := r.rt.Call(context.Background(), shareSetReq("none"))
		var unknown *OutcomeUnknownError
		if errors.As(err, &unknown) || proto.CodeOf(err) != proto.CodeWorkerOffline {
			t.Fatalf("err = %#v: nothing was sent, so the outcome is not unknown", err)
		}
	})
	t.Run("a-read-that-is-lost-is-still-worker-offline", func(t *testing.T) {
		r := newRig(t, nil)
		r.be.online[wAlicePi1.ID] = Online{WorkerID: wAlicePi1.ID, Ops: []string{ops.OpShareGet}}
		r.script(newFakeStream(readItem{err: io.ErrUnexpectedEOF}))
		_, err := r.rt.Call(context.Background(), CallRequest{
			Principal: routeAlice(), Op: ops.OpShareGet, WorkerID: wAlicePi1.ID, Args: &ops.ShareGetArgs{WorkerID: wAlicePi1.ID},
		})
		var unknown *OutcomeUnknownError
		if errors.As(err, &unknown) || proto.CodeOf(err) != proto.CodeWorkerOffline {
			t.Fatalf("err = %#v: a read has no outcome to be unknown", err)
		}
	})
}
