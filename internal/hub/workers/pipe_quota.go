package workers

// Ingestion quotas of the turn channel (AD-23).
//
// A turn is the reply of a Worker to a message the Hub delivered to one of its
// sessions, and nothing else. Before anything is written the Hub decides in
// this order (see [Pipe.IngestTurn]):
//
//  1. the frame rate of the Worker, charged for every turn frame, valid or
//     not: it protects the database reads and the CPU of the Hub from a Worker
//     that floods the channel. Nothing else is charged to a frame that fails
//     the next steps, so garbage cannot use the capacity of the owner or of
//     the other Workers;
//  2. the turn itself: size, UTF-8, session name, and the delivery it answers.
//     The delivery must be a message of the same Worker and session that has
//     no reply yet; the identity is the Worker the handshake authenticated,
//     never a field of the frame. A turn that fails is rejected for good
//     (status rejected, the Worker drops it); a turn already stored gets its
//     lost ack again (stored) without costing capacity;
//  3. the capacity of the turn, charged only now: bytes and turns per second
//     of the Worker, of its owner and of the Hub, the volume stored in the
//     database for the Worker, the owner and the Hub (the quota of
//     migration 0016, checked in the transaction of the insert) and the size
//     of the database and its write-ahead log. A turn that does not fit is not
//     stored and gets status busy with retry_after_ms: the Worker keeps it and
//     sends it again later, within its own bounds (see the Worker's turn
//     emitter).
//
// A Worker that keeps sending after busy answers is not served for ever, but
// the rule is about the Worker, not about the stream, and only about a turn that
// comes EARLY. A busy answer holds the Worker until its retry_after (the hold
// belongs to the Worker and outlives a stream). A frame that arrives while the
// hold is on and is answered busy again is an early frame; after
// [maxBusyStrikes] of them the stream is reset, and the connection is closed
// when that happens again and again. A first attempt, and a retry made after the
// hold, are never early, however long the Worker has been told to wait: a
// Worker with a backlog during a stored-volume or pressure period is told to
// wait again and again and keeps its connection as long as it waits. The few
// frames that were already in flight when the first busy answer was sent are
// early too, and far below the bound (the Worker's window is
// agent.DefaultMaxInFlight).

import (
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Default ingestion limits (see [TurnQuotas]).
const (
	DefaultWorkerFrameRate  = 50.0
	DefaultWorkerFrameBurst = 100.0
	// The bytes of every inbound frame of the turn channel, whatever it holds:
	// well formed or not, a turn or an unknown type.
	DefaultWorkerFrameByteRate  = 4 << 20
	DefaultWorkerFrameByteBurst = 8 << 20

	DefaultWorkerTurnRate  = 5.0
	DefaultWorkerTurnBurst = 20.0
	DefaultWorkerByteRate  = 1 << 20
	DefaultWorkerByteBurst = 2 << 20

	DefaultOwnerTurnRate  = 20.0
	DefaultOwnerTurnBurst = 80.0
	DefaultOwnerByteRate  = 4 << 20
	DefaultOwnerByteBurst = 8 << 20

	DefaultGlobalTurnRate  = 100.0
	DefaultGlobalTurnBurst = 400.0
	DefaultGlobalByteRate  = 16 << 20
	DefaultGlobalByteBurst = 32 << 20

	// The stored volume is the quota of the retention window (7 days).
	DefaultWorkerStoredBytes = 128 << 20
	DefaultWorkerStoredTurns = 100_000
	DefaultOwnerStoredBytes  = 512 << 20
	DefaultOwnerStoredTurns  = 400_000
	DefaultGlobalStoredBytes = 2 << 30
	DefaultGlobalStoredTurns = 1_600_000

	DefaultMaxDBBytes  = 16 << 30
	DefaultMaxWALBytes = 256 << 20

	// quotaRetryAfter is what a stored-volume or pressure refusal tells the
	// Worker: retention frees volume hourly, a checkpoint within minutes.
	quotaRetryAfter = time.Minute
	// maxJunkFrames is how many frames in a row that are not a turn (unknown
	// types, frames that do not parse) a stream may send before it is reset. A
	// single unknown frame is forward compatibility (AD-4); a run of them is a
	// flood, and each of them has been charged to the frame budget anyway.
	maxJunkFrames = 64
	// maxBusyStrikes is how many early frames answered busy a Worker may send
	// before its stream is reset; abuseStrikes resets within abuseWindow close
	// the connection.
	maxBusyStrikes = 32
	abuseStrikes   = 3
	abuseWindow    = 10 * time.Minute
	pressureTTL    = time.Second
	limiterIdle    = 10 * time.Minute
)

// TurnQuotas bounds what the Workers may store through the turn channel. A zero
// member selects its default.
type TurnQuotas struct {
	// Frames per second of one Worker, with the burst, and the bytes of those
	// frames per second, with the burst. Every inbound frame is charged to both
	// before anything is decided about it.
	WorkerFrameRate, WorkerFrameBurst         float64
	WorkerFrameByteRate, WorkerFrameByteBurst float64
	// Turns and bytes per second, with bursts, of one Worker, of one owner and
	// of the Hub.
	WorkerTurnRate, WorkerTurnBurst float64
	WorkerByteRate, WorkerByteBurst float64
	OwnerTurnRate, OwnerTurnBurst   float64
	OwnerByteRate, OwnerByteBurst   float64
	GlobalTurnRate, GlobalTurnBurst float64
	GlobalByteRate, GlobalByteBurst float64
	// The stored volume (the sum of store.TurnCost and the number of turns in
	// the database) of one Worker, of one owner and of the Hub.
	Stored store.TurnLimits
	// MaxDBBytes and MaxWALBytes: while the database or its WAL is larger, no
	// turn is accepted.
	MaxDBBytes, MaxWALBytes int64
}

func (q TurnQuotas) withDefaults() TurnQuotas {
	f := func(p *float64, d float64) {
		if *p <= 0 {
			*p = d
		}
	}
	i := func(p *int64, d int64) {
		if *p <= 0 {
			*p = d
		}
	}
	f(&q.WorkerFrameRate, DefaultWorkerFrameRate)
	f(&q.WorkerFrameBurst, DefaultWorkerFrameBurst)
	f(&q.WorkerFrameByteRate, DefaultWorkerFrameByteRate)
	f(&q.WorkerFrameByteBurst, DefaultWorkerFrameByteBurst)
	f(&q.WorkerTurnRate, DefaultWorkerTurnRate)
	f(&q.WorkerTurnBurst, DefaultWorkerTurnBurst)
	f(&q.WorkerByteRate, DefaultWorkerByteRate)
	f(&q.WorkerByteBurst, DefaultWorkerByteBurst)
	f(&q.OwnerTurnRate, DefaultOwnerTurnRate)
	f(&q.OwnerTurnBurst, DefaultOwnerTurnBurst)
	f(&q.OwnerByteRate, DefaultOwnerByteRate)
	f(&q.OwnerByteBurst, DefaultOwnerByteBurst)
	f(&q.GlobalTurnRate, DefaultGlobalTurnRate)
	f(&q.GlobalTurnBurst, DefaultGlobalTurnBurst)
	f(&q.GlobalByteRate, DefaultGlobalByteRate)
	f(&q.GlobalByteBurst, DefaultGlobalByteBurst)
	i(&q.Stored.WorkerCost, DefaultWorkerStoredBytes)
	i(&q.Stored.WorkerTurns, DefaultWorkerStoredTurns)
	i(&q.Stored.OwnerCost, DefaultOwnerStoredBytes)
	i(&q.Stored.OwnerTurns, DefaultOwnerStoredTurns)
	i(&q.Stored.GlobalCost, DefaultGlobalStoredBytes)
	i(&q.Stored.GlobalTurns, DefaultGlobalStoredTurns)
	i(&q.MaxDBBytes, DefaultMaxDBBytes)
	i(&q.MaxWALBytes, DefaultMaxWALBytes)
	// A frame of the largest size must fit the byte budget of the frames.
	q.WorkerFrameByteBurst = max(q.WorkerFrameByteBurst, float64(proto.StreamFrameLimit))
	// A turn of the largest size must fit a burst, or it could never be taken.
	maxCost := float64(store.TurnCost(MaxTurnBytes))
	for _, b := range []*float64{&q.WorkerByteBurst, &q.OwnerByteBurst, &q.GlobalByteBurst} {
		if *b < maxCost {
			*b = maxCost
		}
	}
	return q
}

// bucket is a token bucket; it is used under the lock of its limiter.
type bucket struct {
	tokens, rate, burst float64
	last                time.Time
}

func newBucket(rate, burst float64, now time.Time) *bucket {
	return &bucket{tokens: burst, rate: rate, burst: burst, last: now}
}

func (b *bucket) refill(now time.Time) {
	if d := now.Sub(b.last).Seconds(); d > 0 {
		b.tokens = min(b.burst, b.tokens+d*b.rate)
		b.last = now
	}
}

// wait is how long until n tokens are there; zero when they are.
func (b *bucket) wait(n float64) time.Duration {
	if b.tokens >= n {
		return 0
	}
	return time.Duration((n - b.tokens) / b.rate * float64(time.Second))
}

// meter is a turn bucket and a byte bucket that are taken together.
type meter struct{ turns, bytes *bucket }

func (m *meter) idle(now time.Time) bool {
	m.turns.refill(now)
	m.bytes.refill(now)
	return m.turns.tokens >= m.turns.burst && m.bytes.tokens >= m.bytes.burst
}

// turnLimiter holds the rate buckets of the Workers, the owners and the Hub.
type turnLimiter struct {
	q   TurnQuotas
	now func() time.Time

	mu      sync.Mutex
	frames  map[string]*meter // frame count (turns) and frame bytes of a Worker
	workers map[string]*meter
	owners  map[string]*meter
	global  *meter
	strikes map[string][]time.Time
	// holds are the busy holds of the Workers (see arrive): bounded like the
	// other maps, an entry ends with its hold.
	holds map[string]*busyHold
}

// busyHold is the time until which a Worker was told to wait, and how many
// frames it sent before that time and had answered busy again.
type busyHold struct {
	until time.Time
	early int
}

func newTurnLimiter(q TurnQuotas, now func() time.Time) *turnLimiter {
	q = q.withDefaults()
	t := now()
	return &turnLimiter{
		q: q, now: now,
		frames: map[string]*meter{}, workers: map[string]*meter{}, owners: map[string]*meter{},
		global:  &meter{newBucket(q.GlobalTurnRate, q.GlobalTurnBurst, t), newBucket(q.GlobalByteRate, q.GlobalByteBurst, t)},
		strikes: map[string][]time.Time{},
		holds:   map[string]*busyHold{},
	}
}

// arrive is called when a turn frame of the Worker arrives, before it is
// decided. It reports whether the frame is early: a hold that an earlier busy
// answer announced is still on. A frame at or after the end of the hold is a
// compliant retry and starts the count again.
func (l *turnLimiter) arrive(workerID string) (early bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.holds[workerID]
	if h == nil {
		return false
	}
	if l.now().Before(h.until) {
		return true
	}
	delete(l.holds, workerID)
	return false
}

// busy records a busy answer that asks the Worker to wait: the hold lasts at
// least until now+wait. It returns how many early frames the Worker has sent
// during the current hold (early says whether the frame this answer is for was
// early).
func (l *turnLimiter) busy(workerID string, early bool, wait time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	h := l.holds[workerID]
	if h == nil {
		if len(l.holds) >= 1024 {
			for id, o := range l.holds {
				if !now.Before(o.until) {
					delete(l.holds, id)
				}
			}
		}
		h = &busyHold{}
		l.holds[workerID] = h
	}
	if until := now.Add(wait); until.After(h.until) {
		h.until = until
	}
	if early {
		h.early++
	}
	return h.early
}

// retryAfter turns a wait into the retry_after_ms of an answer: at least one
// second, so that a Worker does not come back at once.
func retryAfter(d time.Duration) int64 {
	return max(d, time.Second).Milliseconds()
}

// frame charges one inbound frame of size bytes to the Worker; it reports how
// long the Worker must wait when it is over its frame rate or its frame bytes.
// Nothing is taken from either budget when the frame does not fit, so a Worker
// that keeps sending does not push the time it must wait further out.
func (l *turnLimiter) frame(workerID string, size int) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	m := l.frames[workerID]
	if m == nil {
		l.sweepLocked(now)
		m = &meter{newBucket(l.q.WorkerFrameRate, l.q.WorkerFrameBurst, now), newBucket(l.q.WorkerFrameByteRate, l.q.WorkerFrameByteBurst, now)}
		l.frames[workerID] = m
	}
	m.turns.refill(now)
	m.bytes.refill(now)
	n := float64(size)
	if w := max(m.turns.wait(1), m.bytes.wait(n)); w > 0 {
		return w, false
	}
	m.turns.tokens--
	m.bytes.tokens -= n
	return 0, true
}

// admit charges a turn of cost bytes to the Worker, its owner and the Hub, all
// or nothing. The returned function gives the charge back (the turn was not
// stored after all).
func (l *turnLimiter) admit(workerID, ownerID string, cost int) (refund func(), wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)
	w := l.workers[workerID]
	if w == nil {
		w = &meter{newBucket(l.q.WorkerTurnRate, l.q.WorkerTurnBurst, now), newBucket(l.q.WorkerByteRate, l.q.WorkerByteBurst, now)}
		l.workers[workerID] = w
	}
	ms := []*meter{w}
	if ownerID != "" {
		o := l.owners[ownerID]
		if o == nil {
			o = &meter{newBucket(l.q.OwnerTurnRate, l.q.OwnerTurnBurst, now), newBucket(l.q.OwnerByteRate, l.q.OwnerByteBurst, now)}
			l.owners[ownerID] = o
		}
		ms = append(ms, o)
	}
	ms = append(ms, l.global)
	n := float64(cost)
	var longest time.Duration
	for _, m := range ms {
		m.turns.refill(now)
		m.bytes.refill(now)
		longest = max(longest, m.turns.wait(1), m.bytes.wait(n))
	}
	if longest > 0 {
		return nil, longest, false
	}
	for _, m := range ms {
		m.turns.tokens--
		m.bytes.tokens -= n
	}
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, m := range ms {
			m.turns.tokens = min(m.turns.burst, m.turns.tokens+1)
			m.bytes.tokens = min(m.bytes.burst, m.bytes.tokens+n)
		}
	}, 0, true
}

// sweepLocked forgets the Workers and owners whose buckets are full again, so
// that the maps do not grow with every Worker that ever connected.
func (l *turnLimiter) sweepLocked(now time.Time) {
	if len(l.frames)+len(l.workers)+len(l.owners) < 1024 {
		return
	}
	for id, m := range l.frames {
		if m.idle(now) {
			delete(l.frames, id)
		}
	}
	for id, m := range l.workers {
		if m.idle(now) {
			delete(l.workers, id)
		}
	}
	for id, m := range l.owners {
		if m.idle(now) {
			delete(l.owners, id)
		}
	}
}

// strike records that the Worker's stream was reset for ignoring busy answers
// and reports whether the connection has now earned its end.
func (l *turnLimiter) strike(workerID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := l.strikes[workerID][:0]
	for _, t := range l.strikes[workerID] {
		if now.Sub(t) < abuseWindow {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	if h := l.holds[workerID]; h != nil {
		h.early = 0 // the next stream starts its own count
	}
	if len(keep) >= abuseStrikes {
		delete(l.strikes, workerID)
		return true
	}
	l.strikes[workerID] = keep
	return false
}
