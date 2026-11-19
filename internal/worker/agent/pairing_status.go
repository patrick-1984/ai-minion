package agent

import (
	"sync"
	"time"
)

// FeaturePairingStatus is the local capability of a Worker whose status
// carries the pairing code it shows, with its attempt and expiry (tray W8a).
// A client that finds it may read the code from "minion worker status --json"
// instead of from the console.
const FeaturePairingStatus = "pairing_status.v1"

// PendingPairing is the pairing code a Worker shows and waits for.
type PendingPairing struct {
	// Code is the pairing code, for example MN-K7Q4-XY12. It is a secret
	// that is shown to the human who owns the Worker (AD-20); the local
	// status sends it to the OS user that owns the control socket only.
	Code string
	// Attempt identifies this code: it changes whenever the code changes.
	Attempt string
	// ExpiresAt is when the code stops being valid.
	ExpiresAt time.Time
}

// PairingStatus is the status source of the Worker's own pairing (the
// auto-pairing of the connector module). The [Pairer] feeds it through its
// OnCode and OnDone hooks, and the local socket reads it for the status.
// The zero value is not usable; use [NewPairingStatus].
type PairingStatus struct {
	mu  sync.Mutex
	cur PendingPairing
	now func() time.Time
}

// NewPairingStatus returns a source with no code pending.
func NewPairingStatus() *PairingStatus {
	return &PairingStatus{now: time.Now}
}

// OnCode records the code that is shown now. An empty code means the code
// shown before was retired and no new one is shown yet.
func (s *PairingStatus) OnCode(code, attempt string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" {
		s.cur = PendingPairing{}
		return
	}
	s.cur = PendingPairing{Code: code, Attempt: attempt, ExpiresAt: expires}
}

// OnDone records that the pairing run ended: paired, stopped or failed.
func (s *PairingStatus) OnDone() {
	s.mu.Lock()
	s.cur = PendingPairing{}
	s.mu.Unlock()
}

// Pending returns the code that is shown and still valid, if any. A code
// whose expiry passed is not reported, even before the pairer replaced it.
func (s *PairingStatus) Pending() (PendingPairing, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.Code == "" {
		return PendingPairing{}, false
	}
	if !s.cur.ExpiresAt.IsZero() && !s.now().Before(s.cur.ExpiresAt) {
		return PendingPairing{}, false
	}
	return s.cur, true
}
