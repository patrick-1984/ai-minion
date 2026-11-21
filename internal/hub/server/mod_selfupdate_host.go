package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/selfupdate/truenas"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Paths are deployment configuration, never browser-supplied. The status
// directory is mounted read-only; only the request directory is writable.
type hostUpdater struct {
	mu                      sync.Mutex
	requestFile, statusFile string
}

func hostUpdaterFromEnv() *hostUpdater {
	req, status := os.Getenv("MINION_HUB_UPDATER_REQUEST_FILE"), os.Getenv("MINION_HUB_UPDATER_STATUS_FILE")
	if req == "" && status == "" {
		return nil
	}
	return &hostUpdater{requestFile: req, statusFile: status}
}

func (h *hostUpdater) read(now time.Time) *ops.HubHostUpdaterStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readLocked(now)
}

func (h *hostUpdater) readLocked(now time.Time) *ops.HubHostUpdaterStatus {
	r := &ops.HubHostUpdaterStatus{}
	if !filepath.IsAbs(h.requestFile) || !filepath.IsAbs(h.statusFile) {
		return r
	}
	s, err := truenas.ReadStatus(h.statusFile)
	if err != nil {
		return r
	}
	r.Available, r.App, r.Tick, r.Detail = true, s.App, s.Tick, s.Detail
	r.Stale = now.Sub(s.UpdatedAt) > 45*time.Minute || s.UpdatedAt.After(now.Add(time.Minute))
	r.UpdatedAt = s.UpdatedAt.UTC().Format(time.RFC3339)
	r.Enabled, r.Window, r.Channel, r.AllowHubPolicy = s.Enabled, s.Window, s.Channel, s.AllowHubPolicy
	r.RequestError = s.RequestError
	if s.LastAttempt != nil {
		r.Attempt, r.Outcome = s.LastAttempt.Phase, s.LastAttempt.Outcome
	}
	if req, _ := truenas.ReadRequest(h.requestFile, now); req != nil {
		if req.Policy != nil {
			r.PolicyRevision = req.Policy.Revision
			if s.AllowHubPolicy {
				r.Enabled, r.Window, r.Channel = req.Policy.Enabled, req.Policy.Window, req.Policy.Channel
			}
		}
		if req.Once != nil && s.RequestHandled != req.Once.ID {
			r.PendingRequest = req.Once.Target
		}
	}
	return r
}

func (h *hostUpdater) request(now time.Time) (truenas.Request, error) {
	r, err := truenas.ReadRequest(h.requestFile, now)
	if err != nil && err.Error() != "request_expired" {
		return truenas.Request{}, errors.New("the host update request cannot be read")
	}
	if r == nil {
		return truenas.Request{Version: truenas.RequestVersion}, nil
	}
	return *r, nil
}

func (h *hostUpdater) policy(a ops.HubUpdatePolicyArgs, now time.Time) error {
	if _, err := truenas.ParseWindow(a.Window); err != nil {
		return errors.New("use a UTC window HH-HH, for example 03-05")
	}
	if _, err := truenas.ParseChannel(a.Channel); err != nil {
		return errors.New("use the channel latest, X or X.Y")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.readLocked(now)
	if !s.Available || !s.AllowHubPolicy || (a.Enabled && s.Stale) {
		return errors.New("the host updater must be online and allow Hub policy before arming it")
	}
	r, err := h.request(now)
	if err != nil {
		return err
	}
	rev := int64(0)
	if r.Policy != nil {
		rev = r.Policy.Revision
	}
	if a.Revision != rev {
		return errors.New("the update policy changed; reload the page and try again")
	}
	r.Policy = &truenas.Policy{Enabled: a.Enabled, Window: a.Window, Channel: a.Channel, Revision: rev + 1}
	// A policy change cancels any queued one-shot; it must not silently
	// survive a decision to disable or change the update destination.
	r.Once = nil
	if err := truenas.WriteRequest(h.requestFile, r); err != nil {
		return errors.New("the host update request could not be saved; check its directory mount")
	}
	return nil
}

func (h *hostUpdater) queue(baseline, target string, now time.Time) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.readLocked(now)
	if !s.Available || s.Stale || s.Tick == "recovery_required" || s.Tick == "reconcile_pending" {
		return errors.New("the host updater is unavailable, stale or recovering")
	}
	if s.PendingRequest != "" {
		return errors.New("a host update is already queued")
	}
	r, err := h.request(now)
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return errors.New("an update request could not be created")
	}
	r.Once = &truenas.Once{ID: hex.EncodeToString(id[:]), Baseline: baseline, Target: target, ExpiresAt: now.Add(time.Hour)}
	if r.Policy != nil {
		r.Once.PolicyRevision = r.Policy.Revision
	}
	if err := truenas.WriteRequest(h.requestFile, r); err != nil {
		return errors.New("the host update request could not be saved; check its directory mount")
	}
	return nil
}
