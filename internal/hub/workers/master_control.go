package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// MasterControl holds only live authenticated link state. The stored name
// slot and its generation remain authoritative across Hub restarts.
type MasterControl struct {
	db       *store.Store
	registry *Registry
	events   *EventHub
	router   *Router
	auditor  *audit.Log
	now      func() time.Time

	mu    sync.Mutex
	lanes map[string]*masterLane
}

type masterLease struct {
	binding proto.MasterBinding
	row     store.BoundMasterRow
	// master is true for a lease that stands on an active nomination. A lease
	// with master false stands only on the verified session grants of that
	// incarnation; its row is not a stored nomination.
	master bool
}

// grantLeaseGeneration is the generation of a lease that stands only on
// verified session grants (it has no nomination and so no stored generation).
const grantLeaseGeneration = 1

type masterLane struct {
	info    Online
	ctx     context.Context
	end     context.CancelFunc
	report  proto.MasterReport
	expires time.Time
	serial  uint64
	leases  map[string]masterLease
	updates chan proto.MasterState
	pending map[*masterPending]struct{}
}

type masterPending struct {
	incarnation string
	cancel      context.CancelFunc
}

func NewMasterControl(db *store.Store, registry *Registry, events *EventHub, router *Router, auditor *audit.Log) *MasterControl {
	return &MasterControl{db: db, registry: registry, events: events, router: router, auditor: auditor,
		now: time.Now, lanes: map[string]*masterLane{}}
}

func (m *MasterControl) Bind(ingest *Ingest, service *Service) error {
	if !ingest.OnStream(proto.TypeMasterOpen, m.serveLane) ||
		!service.HandleCallOp(proto.OpMasterCallV1, m.serveMasterCall) {
		return errors.New("workers: the master control lane is already registered")
	}
	return nil
}

func (m *MasterControl) currentLink(info Online) bool {
	current, ok := m.registry.Get(info.WorkerID)
	return ok && !current.Pending && current.MasterSessionV1 && info.MasterSessionV1 && current.OwnerID == info.OwnerID &&
		m.registry.hasLiveLink(info.WorkerID, info.TranscriptHash)
}

func (m *MasterControl) serveLane(ctx context.Context, info Online, st *transport.Stream, first json.RawMessage) {
	if !m.currentLink(info) {
		st.Reset()
		return
	}
	ctx, end := context.WithCancel(ctx)
	defer end()
	defer interruptOnDone(ctx, st)()
	lane := &masterLane{info: info, ctx: ctx, end: end, leases: map[string]masterLease{},
		updates: make(chan proto.MasterState, 1), pending: map[*masterPending]struct{}{}}
	if !m.applyReport(lane, first, true) {
		st.Reset()
		return
	}
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		for {
			select {
			case <-ctx.Done():
				return
			case state := <-lane.updates:
				_ = st.SetWriteDeadline(time.Now().Add(masterIOTimeout))
				if err := st.WriteFrame(state); err != nil {
					end()
					return
				}
			}
		}
	}()
	defer func() {
		end()
		m.mu.Lock()
		if m.lanes[info.WorkerID] == lane {
			delete(m.lanes, info.WorkerID)
		}
		for p := range lane.pending {
			p.cancel()
		}
		m.mu.Unlock()
		st.Reset()
		<-writer
	}()
	for ctx.Err() == nil {
		deadline := time.Time{}
		if len(lane.report.Candidates) != 0 {
			deadline = time.Now().Add(proto.MasterLeaseTTL)
		}
		_ = st.SetReadDeadline(deadline)
		body, err := st.ReadFrame()
		if err != nil || !m.applyReport(lane, body, false) {
			return
		}
	}
}

const masterIOTimeout = 3 * time.Second

func (m *MasterControl) applyReport(lane *masterLane, body []byte, opening bool) bool {
	var report proto.MasterReport
	if err := proto.DecodeMasterJSON(body, &report); err != nil || report.Validate() != nil {
		return false
	}
	wantType := proto.TypeMasterReport
	if opening {
		wantType = proto.TypeMasterOpen
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if report.Type != wantType || report.Seq <= lane.report.Seq || lane.ctx.Err() != nil || !m.currentLink(lane.info) {
		return false
	}
	if opening {
		if old := m.lanes[lane.info.WorkerID]; old != nil && old != lane {
			old.end()
		}
		m.lanes[lane.info.WorkerID] = lane
	} else if m.lanes[lane.info.WorkerID] != lane {
		return false
	}
	lane.report = report
	if !m.now().Before(lane.expires) {
		// A report after expiration always gets new handles.
		clear(lane.leases)
	}
	lane.expires = m.now().Add(proto.MasterLeaseTTL)
	m.pushLocked(lane)
	return true
}

func (m *MasterControl) pushLocked(l *masterLane) {
	left := l.expires.Sub(m.now()).Milliseconds()
	l.serial++
	state := proto.MasterState{
		Type: proto.TypeMasterState, Serial: l.serial, ReportSeq: l.report.Seq,
		TTLMS: max(1, min(left, proto.MasterLeaseTTL.Milliseconds())), Active: []proto.MasterBinding{},
	}
	next := map[string]masterLease{}
	if left > 0 && l.ctx.Err() == nil && m.currentLink(l.info) {
		ctx, cancel := context.WithTimeout(l.ctx, masterIOTimeout)
		rows, err := m.db.ListBoundMasters(ctx, l.info.WorkerID)
		cancel()
		if err == nil {
			for _, row := range rows {
				if !row.Active || row.OwnerID != l.info.OwnerID || !bytes.Equal(row.WorkerPub, l.info.EnrollmentKey[:]) ||
					!m.eligible(l, row.SessionName, row.IncarnationID) {
					continue
				}
				lease := l.leases[row.IncarnationID]
				if !lease.master || lease.binding.Generation != row.Generation {
					lease = masterLease{binding: proto.MasterBinding{
						IncarnationID: row.IncarnationID, Generation: row.Generation, LeaseID: ops.NewUUIDv7(),
					}, row: row, master: true}
				}
				next[row.IncarnationID] = lease
				state.Active = append(state.Active, lease.binding)
			}
			m.grantLeasesLocked(l, next, &state)
		}
	}
	for pending := range l.pending {
		if next[pending.incarnation].binding != l.leases[pending.incarnation].binding {
			pending.cancel()
			delete(l.pending, pending)
		}
	}
	l.leases = next
	select {
	case <-l.updates:
	default:
	}
	l.updates <- state
}

// grantLeasesLocked adds a lease for every reported candidate that is not a
// nominated master but whose (Worker, session, incarnation) holds at least one
// verified session grant. The store checks the owner and the enrollment key
// of the lane, eligible the report and the share, as for a nomination.
func (m *MasterControl) grantLeasesLocked(l *masterLane, next map[string]masterLease, state *proto.MasterState) {
	ctx, cancel := context.WithTimeout(l.ctx, masterIOTimeout)
	sources, err := m.db.VerifiedGrantSources(ctx, l.info.WorkerID, l.info.OwnerID, l.info.EnrollmentKey[:])
	cancel()
	if err != nil {
		return
	}
	for _, src := range sources {
		if _, held := next[src.IncarnationID]; held || !m.eligible(l, src.SessionName, src.IncarnationID) {
			continue
		}
		lease := l.leases[src.IncarnationID]
		if lease.master || lease.binding.LeaseID == "" || lease.row.SessionName != src.SessionName {
			lease = masterLease{binding: proto.MasterBinding{
				IncarnationID: src.IncarnationID, Generation: grantLeaseGeneration, LeaseID: ops.NewUUIDv7(),
			}, row: store.BoundMasterRow{
				WorkerID: l.info.WorkerID, SessionName: src.SessionName, IncarnationID: src.IncarnationID,
				OwnerID: l.info.OwnerID, WorkerPub: bytes.Clone(l.info.EnrollmentKey[:]), Generation: grantLeaseGeneration,
			}}
		}
		next[src.IncarnationID] = lease
		state.Active = append(state.Active, lease.binding)
	}
}

// PreparedIncarnation returns the incarnation id that the Worker's master
// lane reports right now for the session: the kernel-proven terminal a grant
// can be bound to. ok is false when the Worker has no live lane, is not
// enrolled as stored, or the session is not a prepared shared terminal (or is
// reported with more than one incarnation).
func (m *MasterControl) PreparedIncarnation(ctx context.Context, workerID, session string) (incarnation string, ok bool) {
	w, err := m.db.WorkerByID(ctx, workerID)
	if err != nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lanes[workerID]
	if l == nil || l.ctx.Err() != nil || !m.currentLink(l.info) || !m.now().Before(l.expires) ||
		l.info.OwnerID != w.OwnerID || !bytes.Equal(l.info.EnrollmentKey[:], w.Pub) {
		return "", false
	}
	for _, c := range l.report.Candidates {
		if c.SessionName != session || !m.eligible(l, c.SessionName, c.IncarnationID) {
			continue
		}
		if incarnation != "" && incarnation != c.IncarnationID {
			return "", false
		}
		incarnation = c.IncarnationID
	}
	return incarnation, incarnation != ""
}

// RefreshLeases recomputes the leases of the Worker's lane now, after its
// verified session grants changed (bound, unbound, revoked). Without it the
// next report of the Worker does the same within its interval.
func (m *MasterControl) RefreshLeases(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.lanes[workerID]; l != nil && l.ctx.Err() == nil {
		m.pushLocked(l)
	}
}

// eligible uses the present ordinary share feed. The database transaction
// independently checks current confirmed and pending share restrictions.
func (m *MasterControl) eligible(l *masterLane, name, incarnation string) bool {
	for _, c := range l.report.Candidates {
		if c.SessionName == name && c.IncarnationID == incarnation {
			return m.events.masterShare(l.info.WorkerID, name)
		}
	}
	return false
}

func (m *MasterControl) SetBoundMaster(ctx context.Context, w store.WorkerRow, a ops.ControlSetBoundMasterArgs, actor string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.lanes[w.ID]
	verified := l != nil && l.ctx.Err() == nil && m.currentLink(l.info) && m.now().Before(l.expires) &&
		l.info.OwnerID == w.OwnerID && bytes.Equal(l.info.EnrollmentKey[:], w.Pub) && m.eligible(l, a.SessionName, a.IncarnationID)
	changed, err := m.db.SetBoundMaster(ctx, w, a, actor, m.now().UnixMilli(), verified)
	if err == nil && changed && l != nil {
		m.pushLocked(l)
	}
	return changed, err
}

// MasterView returns the admin-facing evidence and never includes lease IDs.
func (m *MasterControl) MasterView(ctx context.Context) (map[string]MasterWorkerView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.db.ListBoundMasters(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[string]MasterWorkerView{}
	type slot struct{ worker, name string }
	known := map[slot]store.BoundMasterRow{}
	for _, r := range rows {
		known[slot{r.WorkerID, r.SessionName}] = r
		v := out[r.WorkerID]
		state := "inactive"
		if r.Active {
			state = "waiting"
			if on, ok := m.registry.Get(r.WorkerID); !ok {
				state = "offline"
			} else if !on.MasterSessionV1 {
				state = "unsupported"
			} else if l := m.lanes[r.WorkerID]; l != nil && l.ctx.Err() == nil && m.currentLink(l.info) && m.now().Before(l.expires) {
				if lease := l.leases[r.IncarnationID]; lease.binding.Generation == r.Generation && m.eligible(l, r.SessionName, r.IncarnationID) {
					state = "active"
				} else if len(l.report.Candidates) != 0 && !m.eligible(l, r.SessionName, r.IncarnationID) {
					state = "retired"
				}
			}
		}
		v.Bound = append(v.Bound, ops.ControlBoundMaster{
			SessionName: r.SessionName, IncarnationID: r.IncarnationID, Generation: r.Generation, Master: r.Active, Attachment: state,
		})
		out[r.WorkerID] = v
	}
	for id, l := range m.lanes {
		if l.ctx.Err() != nil || !m.currentLink(l.info) || !m.now().Before(l.expires) {
			continue
		}
		v := out[id]
		for _, c := range l.report.Candidates {
			if !m.eligible(l, c.SessionName, c.IncarnationID) {
				continue
			}
			r := known[slot{id, c.SessionName}]
			active := r.Active && r.IncarnationID == c.IncarnationID
			state := "ready"
			if active {
				state = "waiting"
				if l.leases[c.IncarnationID].binding.Generation == r.Generation {
					state = "active"
				}
			}
			v.Candidates = append(v.Candidates, ops.ControlMasterCandidate{MasterCandidate: c, Generation: r.Generation, Master: active, Attachment: state})
		}
		out[id] = v
	}
	return out, nil
}

type MasterWorkerView struct {
	Candidates []ops.ControlMasterCandidate
	Bound      []ops.ControlBoundMaster
}

func (m *MasterControl) authorizeLocked(c Call) (*masterLane, masterLease, error) {
	l := m.lanes[c.Worker.ID]
	if c.Master == nil || l == nil || l.ctx.Err() != nil || !m.now().Before(l.expires) || !m.currentLink(l.info) ||
		!bytes.Equal(l.info.TranscriptHash, c.TranscriptHash) || l.info.OwnerID != c.Worker.OwnerID ||
		!bytes.Equal(l.info.EnrollmentKey[:], c.Worker.Pub) {
		return nil, masterLease{}, errMasterDenied
	}
	lease := l.leases[c.Master.IncarnationID]
	if lease.binding.LeaseID != c.Master.LeaseID || lease.binding.Generation != c.Master.Generation ||
		!m.eligible(l, lease.row.SessionName, c.Master.IncarnationID) {
		return nil, masterLease{}, errMasterDenied
	}
	return l, lease, nil
}

var errMasterDenied = proto.NewError(proto.CodeForbidden, "the session master attachment is inactive or stale")

// Controllers reports attached nominations whose current two-sided grant
// reaches one shared destination session. It makes no terminal calls.
func (m *MasterControl) Controllers(ctx context.Context, workerID, session string) ([]ops.ControlChain, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	on, ok := m.registry.Get(workerID)
	if !ok || on.Pending || !m.registry.hasLiveLink(workerID, on.TranscriptHash) || !m.events.masterShare(workerID, session) {
		return nil, nil
	}
	var out []ops.ControlChain
	for _, l := range m.lanes {
		if l.ctx.Err() != nil || !m.currentLink(l.info) || !m.now().Before(l.expires) {
			continue
		}
		for _, lease := range l.leases {
			// A lease that stands only on verified grants is no nomination.
			if !lease.master || l.info.WorkerID == workerID || !m.eligible(l, lease.row.SessionName, lease.row.IncarnationID) {
				continue
			}
			if _, err := m.db.BoundMasterTargets(ctx, lease.row, &store.MasterTarget{WorkerID: workerID, OwnerID: on.OwnerID, Sessions: []string{session}}); err != nil {
				if masterAuthorityError(err) == errMasterDenied {
					continue
				}
				return nil, err
			}
			out = append(out, ops.ControlChain{WorkerID: l.info.WorkerID, Worker: l.info.Name, SessionName: lease.row.SessionName})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Worker != out[j].Worker {
			return out[i].Worker < out[j].Worker
		}
		return ops.NaturalCompare(out[i].SessionName, out[j].SessionName) < 0
	})
	return out, nil
}
