//go:build linux && !android

package agent

import (
	"context"
	"io"
	"slices"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

type masterLane struct {
	ctx  context.Context
	link *Link
}

type sentMasterReport struct {
	at  time.Time
	ids map[string]bool
}

func (b *masterBroker) serveLink(link *Link) {
	if link.Welcome.Pending || !link.Welcome.MasterSessionV1 {
		return
	}
	ctx := link.Context()
	for ctx.Err() == nil {
		b.runLane(ctx, link)
		select {
		case <-ctx.Done():
			return
		case <-time.After(DefaultReopenDelay):
		}
	}
}

func (b *masterBroker) runLane(parent context.Context, link *Link) {
	stream, err := link.Session.OpenEvents(parent)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(ctx, stream.Reset)
	defer stop()
	defer stream.Reset()
	defer cancel()
	lane := &masterLane{ctx: ctx, link: link}
	if !b.openLane(lane) {
		return
	}
	defer b.closeLane(lane)
	states := make(chan proto.MasterState)
	go func() {
		defer cancel()
		readMasterStates(ctx, stream, states)
	}()
	periodic := time.NewTicker(proto.MasterReportInterval)
	defer periodic.Stop()
	sent := make(map[uint64]sentMasterReport)
	var seq, serial uint64
	var previous []proto.MasterCandidate
	send := func(force bool) error {
		b.reconcile(ctx)
		b.mu.Lock()
		current := b.candidates()
		b.mu.Unlock()
		if seq != 0 && slices.Equal(current, previous) && (!force || len(current) == 0) {
			return nil
		}
		seq++
		typ := proto.TypeMasterReport
		if seq == 1 {
			typ = proto.TypeMasterOpen
		}
		r := proto.MasterReport{Type: typ, Seq: seq, Candidates: current}
		if err := r.Validate(); err != nil {
			return err
		}
		if err := proto.WriteFrame(io.Discard, proto.MasterFrameLimit, r); err != nil {
			return err
		}
		now := time.Now()
		ids := make(map[string]bool, len(current))
		for _, c := range current {
			ids[c.IncarnationID] = true
		}
		sent[seq] = sentMasterReport{at: now, ids: ids}
		for old, report := range sent {
			// Retain the newest 16 send times through the maximum lease
			// window. Unknown or too-old acknowledgements grant nothing.
			if old+16 <= seq || !now.Before(report.at.Add(proto.MasterLeaseTTL)) {
				delete(sent, old)
			}
		}
		previous = current
		_ = stream.SetWriteDeadline(now.Add(2 * time.Second))
		return stream.WriteFrame(r)
	}
	if err := send(true); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case state, ok := <-states:
			if !ok {
				return
			}
			report, exists := sent[state.ReportSeq]
			if exists && state.Serial > serial {
				serial = state.Serial
				b.applyMasterState(lane, state, report)
			}
		case <-b.changed:
			if err := send(false); err != nil {
				return
			}
		case <-periodic.C:
			if err := send(true); err != nil {
				return
			}
		}
	}
}

func readMasterStates(ctx context.Context, stream *transport.Stream, out chan<- proto.MasterState) {
	defer close(out)
	for ctx.Err() == nil {
		body, err := stream.ReadFrame()
		if err != nil {
			return
		}
		var state proto.MasterState
		if proto.DecodeMasterJSON(body, &state) != nil || state.Validate() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case out <- state:
		}
	}
}

func (b *masterBroker) openLane(lane *masterLane) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	owner := linkMasterOwner(lane.link)
	if lane.ctx.Err() != nil || !b.sameOwner(owner) ||
		(b.current != nil && b.current.ctx.Err() == nil) {
		return false
	}
	for _, a := range b.attachments {
		if a.owner != owner {
			b.retire(a)
		} else {
			b.clearLease(a)
		}
	}
	b.current = lane
	return true
}

func (b *masterBroker) closeLane(lane *masterLane) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current == lane {
		b.current = nil
		for _, a := range b.attachments {
			b.clearLease(a)
		}
	}
}

func (b *masterBroker) applyMasterState(lane *masterLane, state proto.MasterState, report sentMasterReport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current != lane || lane.ctx.Err() != nil {
		return
	}
	deadline := report.at.Add(time.Duration(state.TTLMS) * time.Millisecond)
	active := make(map[string]proto.MasterBinding, len(state.Active))
	if time.Now().Before(deadline) {
		for _, lease := range state.Active {
			if report.ids[lease.IncarnationID] {
				active[lease.IncarnationID] = lease
			}
		}
	}
	for _, a := range b.attachments {
		if lease, ok := active[a.incarnation]; ok && b.sameOwner(a.owner) && a.peer != nil {
			if a.lease != nil && a.lease.wire == lease {
				a.lease.until = deadline
				continue
			}
			b.clearLease(a)
			ctx, cancel := context.WithCancel(lane.ctx)
			a.lease = &masterLease{wire: lease, until: deadline, ctx: ctx, cancel: cancel}
		} else {
			b.clearLease(a)
		}
	}
}
