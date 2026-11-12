package hubid

import (
	"context"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// WarnLevel is the severity of the expiry warning (section 8.4).
type WarnLevel string

// The warning levels.
const (
	WarnNone    WarnLevel = ""
	WarnWarning WarnLevel = "warning" // from 30 days before not_after
	WarnError   WarnLevel = "error"   // from 7 days before not_after
	WarnExpired WarnLevel = "expired" // now >= not_after
)

// ScopeStatus is the limit state of one passphrase scope.
type ScopeStatus struct {
	Failures    int64
	Locked      bool
	LockedUntil time.Time
	// AttemptsThisHour counts the attempts of the current allowance window.
	AttemptsThisHour int64
}

// Status is what the identity page shows. It comes from the database and
// needs no key file and no passphrase.
type Status struct {
	State           State
	PassphraseSet   bool
	RootFingerprint string
	OpFingerprint   string
	Serial          uint64
	NotBefore       time.Time
	NotAfter        time.Time
	// DaysLeft is the whole days until not_after (negative once expired).
	DaysLeft       int
	Warning        WarnLevel
	Expired        bool
	MigratedFrom06 bool
	LastOp         string
	LastOpAt       time.Time
	Web, CLI       ScopeStatus
	Generation     uint64
	// Serving is true when Current would return a snapshot.
	Serving bool
}

func (s *Service) scopeStatus(st store.ScopeState, nowMs int64) ScopeStatus {
	out := ScopeStatus{Failures: st.Failures}
	if nowMs < st.LockedUntil {
		out.Locked, out.LockedUntil = true, time.UnixMilli(st.LockedUntil)
	}
	if st.WindowStart != 0 && nowMs >= st.WindowStart && nowMs < st.WindowStart+store.AllowanceWindow {
		out.AttemptsThisHour = st.WindowCount
	}
	return out
}

// Status reads the identity page data. For a Hub with no identity row it
// returns State B0.
func (s *Service) Status(ctx context.Context) (Status, error) {
	row, ok, err := s.st.HubIdentityV2(ctx)
	if err != nil {
		return Status{}, err
	}
	out := Status{Generation: s.Generation()}
	if !ok {
		out.State = StatePristine
		return out, nil
	}
	now := s.nowMs()
	out.RootFingerprint, out.MigratedFrom06, out.PassphraseSet = row.Fingerprint, row.MigratedFrom06, row.RootSealed
	out.LastOp, out.LastOpAt = row.LastOp, time.UnixMilli(row.LastOpAt)
	out.Web, out.CLI = s.scopeStatus(row.Web, now), s.scopeStatus(row.CLI, now)
	switch {
	case !row.RootSealed:
		out.State = StatePlain
	case len(row.CurrentCert) == 0:
		out.State = StateSealedNoCert
	default:
		out.State = StateEstablished
		cert, err := identity.ParseHubCert(row.CurrentCert)
		if err != nil {
			return Status{}, failClosed("the certificate recorded in the database is malformed: %v", err)
		}
		out.OpFingerprint, out.Serial = fingerprint(cert.OpPub), cert.Serial
		out.NotBefore, out.NotAfter = time.UnixMilli(cert.NotBeforeMs).UTC(), time.UnixMilli(cert.NotAfterMs).UTC()
		left := time.Duration(cert.NotAfterMs-now) * time.Millisecond
		out.DaysLeft = int(left / (24 * time.Hour))
		switch {
		case now >= cert.NotAfterMs:
			out.Expired, out.Warning = true, WarnExpired
		case left <= UrgentWindow:
			out.Warning = WarnError
		case left <= WarnWindow:
			out.Warning = WarnWarning
		}
	}
	_, err = s.Current()
	out.Serving = err == nil
	return out, nil
}

// Tick is the timer step of the expiry policy (section 8.4); the Hub calls it
// at start and about every 24 hours (more often is harmless: every event is
// written once). From 30 days before not_after it writes the audit event
// hub_identity.expiry_warning once per UTC day, and at not_after it closes
// every Worker session and writes hub_identity.expired once. Both events have
// the system actor and channel "system" and are unique across restarts. It
// returns the status it acted on.
//
// Tick holds the rotation mutex, like every identity change, so that the
// certificate it judges cannot be replaced between the status read and the
// closing of the sessions: a re-issue that races a tick either completes
// first (the tick then sees the new certificate) or waits for it. MarkExpired
// also refuses a serial that is no longer the current one. When another
// identity operation holds the mutex longer than the lock wait, Tick answers
// CodeConflict and the next tick retries.
func (s *Service) Tick(ctx context.Context) (Status, error) {
	if err := s.lock(ctx); err != nil {
		return Status{}, err
	}
	defer s.unlock()
	st, err := s.Status(ctx)
	if err != nil || st.State != StateEstablished {
		return st, err
	}
	now := s.nowMs()
	// The serving certificate decides, not the newest one in the database: an
	// issuance that committed but is not confirmed leaves the previous pair
	// serving (D3), and that pair expires on its own date.
	if sn := s.snap.Load(); sn != nil && now >= sn.Cert.NotAfterMs && s.gen.Load() == sn.Generation {
		if s.closedGen != sn.Generation {
			s.closedGen = sn.Generation
			s.closeSessions()
			s.log.Error("the Hub certificate has expired; Workers are refused until it is re-issued", "not_after", time.UnixMilli(sn.Cert.NotAfterMs).UTC())
		}
		if int64(sn.Cert.Serial) == int64(st.Serial) {
			row := store.AuditRow{ID: store.NewID(), TS: now, Actor: SystemActor, Action: "hub_identity.expired",
				Target: "channel=system serial=" + itoa(int64(sn.Cert.Serial)) + " not_after=" + time.UnixMilli(sn.Cert.NotAfterMs).UTC().Format(time.RFC3339), Outcome: "success"}
			if _, err := s.st.MarkExpired(ctx, int64(sn.Cert.Serial), row); err != nil {
				return st, err
			}
		}
		return st, nil
	}
	target := "channel=system serial=" + itoa(int64(st.Serial)) + " not_after=" + st.NotAfter.Format(time.RFC3339)
	row := func(action string) store.AuditRow {
		return store.AuditRow{ID: store.NewID(), TS: now, Actor: SystemActor, Action: action, Target: target, Outcome: "success"}
	}
	switch st.Warning {
	case WarnWarning, WarnError:
		if _, err := s.st.MarkExpiryWarning(ctx, now/dayMs+1, row("hub_identity.expiry_warning")); err != nil {
			return st, err
		}
		lvl := s.log.Warn
		if st.Warning == WarnError {
			lvl = s.log.Error
		}
		lvl("the Hub certificate expires soon; re-issue it", "not_after", st.NotAfter, "days_left", st.DaysLeft)
	case WarnExpired:
		// Only reachable when no serving snapshot exists; nothing to close.
		if _, err := s.st.MarkExpired(ctx, int64(st.Serial), row("hub_identity.expired")); err != nil {
			return st, err
		}
	}
	return st, nil
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
