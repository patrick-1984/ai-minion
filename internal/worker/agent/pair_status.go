package agent

import (
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// PairingStatus is the private presentation of a pending pairing code for an
// app that shows it itself (the Android managed mode). The Pairer then writes
// the code here and nowhere else: neither the console box nor the log line of
// the code is produced, so a supervisor that forwards the Worker's output to
// diagnostics or Logcat never sees an active pairing credential. The only
// reader is the local status of the control socket, which only the owning OS
// user can open.
type PairingStatus struct {
	mu  sync.Mutex
	cur *PairingPresentation
	clk transport.Clock
}

// PairingPresentation is what the app shows: the code with its expiry, the
// Worker's own words, and the Hub words as they were presented. Presented
// Hub words are not a verified pin: the pin exists only after the pairing
// succeeded (the CPace transcript covers the Hub's key), so HubVerified stays
// false here and HubExpected tells whether a configured fingerprint will refuse
// any other Hub.
type PairingPresentation struct {
	Code              string `json:"code"`
	ExpiresAt         string `json:"expires_at"`
	WorkerFingerprint string `json:"worker_fingerprint,omitempty"`
	WorkerWords       string `json:"worker_words,omitempty"`
	HubPresented      string `json:"hub_fingerprint_presented,omitempty"`
	HubWordsPresented string `json:"hub_words_presented,omitempty"`
	HubExpected       string `json:"hub_fingerprint_expected,omitempty"`
	HubWordsExpected  string `json:"hub_words_expected,omitempty"`
	HubVerified       bool   `json:"hub_verified"`
	expires           time.Time
}

// NewPairingStatus returns an empty status. A nil clock means the system clock.
func NewPairingStatus(clk transport.Clock) *PairingStatus {
	if clk == nil {
		clk = transport.SystemClock
	}
	return &PairingStatus{clk: clk}
}

func (s *PairingStatus) set(p PairingPresentation) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cur = &p
	s.mu.Unlock()
}

// Clear removes the code: the pairing ended, was cancelled or the code was
// replaced.
func (s *PairingStatus) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cur = nil
	s.mu.Unlock()
}

// setHub records the Hub words that arrived after the code was shown.
func (s *PairingStatus) setHub(root []byte) {
	if s == nil || len(root) == 0 {
		return
	}
	fp := fingerprintOfPub(root)
	s.mu.Lock()
	if s.cur != nil {
		s.cur.HubPresented, s.cur.HubWordsPresented = string(fp), fp.Words()
	}
	s.mu.Unlock()
}

// Current returns the pending presentation, or false when there is none or
// its code expired.
func (s *PairingStatus) Current() (PairingPresentation, bool) {
	if s == nil {
		return PairingPresentation{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || !s.clk.Now().Before(s.cur.expires) {
		return PairingPresentation{}, false
	}
	return *s.cur, true
}

// present records the code of the Pairer in the private status.
func (p *Pairer) present(c identity.PairingCode, root []byte) {
	exp := p.clk.Now().Add(p.ttl)
	pr := PairingPresentation{Code: c.String(), expires: exp, ExpiresAt: exp.UTC().Format(time.RFC3339Nano)}
	if p.fpr != "" {
		pr.WorkerFingerprint, pr.WorkerWords = p.fpr, identity.Fingerprint(p.fpr).Words()
	}
	if len(root) > 0 {
		fp := fingerprintOfPub(root)
		pr.HubPresented, pr.HubWordsPresented = string(fp), fp.Words()
	}
	if !p.cfg.ExpectHub.IsZero() {
		pr.HubExpected, pr.HubWordsExpected = p.cfg.ExpectHub.String(), p.cfg.ExpectHub.Words()
	}
	p.cfg.Private.set(pr)
}
