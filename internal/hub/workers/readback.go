package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// ReadbackTimeout bounds one delivery of the read-back frame: opening the
// stream, the frame and the acknowledgement.
const ReadbackTimeout = 10 * time.Second

// ErrReadbackFenced is returned when the exact connection a delivery was
// planned for is gone, was replaced, belongs to another owner now or is not
// approved: nothing was sent.
var ErrReadbackFenced = errors.New("workers: the connection the usage read-back was built for is no longer current")

// ErrReadbackIneligible is returned when the recipient lost its entitlement
// while the stream was opening: the document was not written. The caller sends
// a clear instead.
var ErrReadbackIneligible = errors.New("workers: the Worker may not receive usage data any more")

// openReadback opens an op stream on the connection described by w, and only
// on that connection: under the registry lock it must still be the registered
// one (same handshake transcript), approved, announce the read-back feature,
// have the owner the document was built for and not be closing. A Worker that
// reconnected, was reassigned or went pending between the caller's look and
// this call gets nothing.
func (r *Registry) openReadback(ctx context.Context, w Online, owner string, clear bool) (*transport.Stream, error) {
	r.mu.Lock()
	e := r.online[w.WorkerID]
	var sess OpSession
	ok := e != nil && !e.info.Pending && len(w.TranscriptHash) == 32 &&
		bytes.Equal(e.info.TranscriptHash, w.TranscriptHash) &&
		e.info.OwnerID == owner && e.info.HasFeature(proto.FeatureUsageReadbackV1)
	if ok {
		sess = e.sess
		select {
		case <-sess.Done():
			ok = false
		default:
		}
	}
	guard := r.readbackGuard
	r.mu.Unlock()
	if !ok {
		return nil, ErrReadbackFenced
	}
	st, err := sess.OpenOp(ctx)
	if err != nil {
		return nil, fmt.Errorf("workers: open the read-back stream: %w", err)
	}
	// Opening can block: the entitlement is checked again now, and a document
	// (not a clear) is written only while it still holds.
	if guard != nil && !clear {
		if err := guard(ctx, w); err != nil {
			st.Reset()
			return nil, err
		}
	}
	return st, nil
}

// SendReadback delivers one read-back frame to the connection w and returns
// the revision the Worker acknowledged after it published the document. The
// frame is the Hub's private control frame (proto.ReadbackApply): it is not an
// op, goes through no router, call principal or origin, and only the Hub's own
// delivery code calls this. A refusal of the Worker is returned as a
// *proto.Error (proto.ReadbackReason names it); a stream that was lost or
// timed out leaves the outcome unknown, and the caller sends the same revision
// again.
func (r *Registry) SendReadback(ctx context.Context, w Online, f proto.ReadbackApply) (proto.ReadbackAck, error) {
	var zero proto.ReadbackAck
	if err := f.Check(); err != nil {
		return zero, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadbackTimeout)
	defer cancel()
	st, err := r.openReadback(ctx, w, f.OwnerID, f.Clear)
	if err != nil {
		return zero, err
	}
	stop := context.AfterFunc(ctx, st.Reset)
	defer stop()
	if err := st.WriteFrame(f); err != nil {
		st.Reset()
		return zero, fmt.Errorf("workers: send the read-back frame: %w", err)
	}
	body, err := readFrame(ctx, st)
	if err != nil {
		st.Reset()
		return zero, fmt.Errorf("workers: read the read-back answer: %w", err)
	}
	typ, _ := proto.FrameType(body)
	switch typ {
	case proto.TypeOpResult:
		var res OpResult
		if err := proto.DecodeFrame(body, &res); err != nil {
			st.Reset()
			return zero, err
		}
		_ = st.Close()
		var ack proto.ReadbackAck
		if err := json.Unmarshal(res.Result, &ack); err != nil {
			return zero, fmt.Errorf("workers: the read-back answer is unusable: %w", err)
		}
		if ack.Revision != f.Revision {
			return zero, fmt.Errorf("workers: the Worker acknowledged revision %d, not %d", ack.Revision, f.Revision)
		}
		return ack, nil
	case proto.TypeError:
		var ef proto.ErrorFrame
		if err := proto.DecodeFrame(body, &ef); err != nil {
			st.Reset()
			return zero, err
		}
		_ = st.Close()
		return zero, ef.Err()
	}
	st.Reset()
	return zero, errors.New("workers: the read-back frame was answered with an unexpected frame")
}
