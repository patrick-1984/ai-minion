package pipe

import (
	"sync"
	"time"
)

// The fixed limits of AD-23. They are the same in Hub and Solo mode and are
// not configurable (only the task budget is, see budget.go).
const (
	// MaxMessageBytes is the largest accepted message: 8 KiB of UTF-8 bytes.
	MaxMessageBytes = 8 * 1024
	// SendBurst is the token bucket capacity per peer.
	SendBurst = 5
	// SendInterval is the token refill interval: 1 message per second.
	SendInterval = time.Second
	// SendsPerHour is the sliding one-hour cap per peer.
	SendsPerHour = 120
	// HourWindow is the window of SendsPerHour.
	HourWindow = time.Hour
	// Retention is how long messages and turns are kept.
	Retention = 7 * 24 * time.Hour

	// maxTrackedPeers bounds the memory of the rate limiter. Peers whose state
	// equals a fresh one are dropped to make room. State that still enforces
	// a limit is never dropped: when every tracked peer is still limited, a
	// new peer is rejected with pipe_rate_limited until one becomes idle, so
	// capacity pressure can never reset a limit.
	maxTrackedPeers = 10000

	// burstCredit is the token bucket capacity in time credit: a send costs
	// SendInterval of credit. Integer time credit keeps RetryAfter exact.
	burstCredit = SendBurst * SendInterval
)

// sendLimiter enforces the per-peer send rate: a token bucket (1/s, burst 5)
// plus a sliding hour window (120). Rejections consume nothing, so a rejected
// message is never counted and never delayed. State is in memory: the hour
// window restarts when the process restarts, while the budget is persistent.
type sendLimiter struct {
	mu    sync.Mutex
	peers map[Peer]*peerState
}

type peerState struct {
	credit time.Duration // token bucket fill; a send costs SendInterval
	last   time.Time     // last refill
	sends  []time.Time   // accepted sends within HourWindow, oldest first
}

func newSendLimiter() *sendLimiter {
	return &sendLimiter{peers: make(map[Peer]*peerState)}
}

// take consumes one send for peer at now, or returns the rejection. The
// RetryAfter of a rejection is the earliest time after which the send can be
// accepted: the larger of the hour window wait and the token wait.
func (l *sendLimiter) take(peer Peer, now time.Time) *Error {
	l.mu.Lock()
	defer l.mu.Unlock()

	st, ok := l.peers[peer]
	if !ok {
		if err := l.makeRoom(now); err != nil {
			return err
		}
		st = &peerState{credit: burstCredit, last: now}
		l.peers[peer] = st
	}
	if el := now.Sub(st.last); el > 0 {
		st.credit += el
		if st.credit > burstCredit {
			st.credit = burstCredit
		}
		st.last = now
	} else if el < 0 {
		st.last = now // clock stepped back: do not mint tokens
	}
	st.trim(now)

	var wait time.Duration
	msg := ""
	if len(st.sends) >= SendsPerHour {
		wait = st.sends[0].Add(HourWindow).Sub(now)
		msg = "hourly send limit reached"
	}
	if st.credit < SendInterval {
		if w := SendInterval - st.credit; w > wait {
			wait = w
			msg = "sending too fast"
		}
	}
	if wait > 0 {
		return &Error{Code: CodePipeRateLimited, RetryAfter: wait, Message: msg}
	}
	st.credit -= SendInterval
	st.sends = append(st.sends, now)
	return nil
}

// release undoes the last successful take of peer at now. The broker calls it
// when storing the message failed, so that a failed write never uses quota.
func (l *sendLimiter) release(peer Peer, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.peers[peer]
	if !ok {
		return
	}
	if n := len(st.sends); n > 0 && st.sends[n-1].Equal(now) {
		st.sends = st.sends[:n-1]
		st.credit += SendInterval
		if st.credit > burstCredit {
			st.credit = burstCredit
		}
	}
}

func (st *peerState) trim(now time.Time) {
	cut := now.Add(-HourWindow)
	i := 0
	for i < len(st.sends) && !st.sends[i].After(cut) {
		i++
	}
	if i > 0 {
		st.sends = append(st.sends[:0], st.sends[i:]...)
	}
}

// idleIn returns how long until the state of st equals a fresh one (full
// bucket, empty hour window); zero means it already does.
func (st *peerState) idleIn(now time.Time) time.Duration {
	var d time.Duration
	if n := len(st.sends); n > 0 {
		d = st.sends[n-1].Add(HourWindow).Sub(now)
	}
	credit := st.credit
	if el := now.Sub(st.last); el > 0 {
		credit += el
	}
	if need := burstCredit - credit; need > d {
		d = need
	}
	if d < 0 {
		d = 0
	}
	return d
}

// makeRoom frees entries so that one more peer fits under maxTrackedPeers. It
// drops only idle peers. If none is idle it returns a pipe_rate_limited error
// whose RetryAfter is the time until the first peer becomes idle.
func (l *sendLimiter) makeRoom(now time.Time) *Error {
	if len(l.peers) < maxTrackedPeers {
		return nil
	}
	var soonest time.Duration
	first := true
	for p, st := range l.peers {
		d := st.idleIn(now)
		if d == 0 {
			delete(l.peers, p)
			continue
		}
		if first || d < soonest {
			soonest, first = d, false
		}
	}
	if len(l.peers) < maxTrackedPeers {
		return nil
	}
	return &Error{Code: CodePipeRateLimited, RetryAfter: soonest,
		Message: "too many active peers, try again later"}
}
