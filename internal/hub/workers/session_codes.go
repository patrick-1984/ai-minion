package workers

// Session codes (v0.23.0): the short "<worker number>-<session number>" code
// of a session ("5-667"), easy to read aloud. The Hub gives the numbers (see
// store.SessionCodes) when it lists sessions: on every sessions_list it
// routes and on the aggregated session list of its event stream, both with
// the instance ids the Worker reports (so a number follows a rename and both
// lists give a session the same code). Only the
// Hub resolves a code, in [Router.Resolve]; a Worker knows nothing of it.

import (
	"context"
	"log/slog"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// SessionCodeStore keeps the numbers; *store.Store implements it.
type SessionCodeStore interface {
	SessionCodes(ctx context.Context, workerID string, seen []store.SessionSighting, now time.Time) (int64, []int64, error)
	WorkerNumbers(ctx context.Context) (map[string]int64, error)
	ResolveSessionCode(ctx context.Context, worker, session int64) (workerID, name string, ok bool, err error)
}

var _ SessionCodeStore = (*store.Store)(nil)

// SessionCodes gives out and resolves session codes. A code is informative:
// a failure to read or write the numbers is logged and leaves the code out,
// it never fails a listing.
type SessionCodes struct {
	Store SessionCodeStore
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Log receives the failures; nil discards them.
	Log *slog.Logger
}

func (c *SessionCodes) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *SessionCodes) warn(msg string, err error) {
	if c.Log != nil {
		c.Log.Warn(msg, "err", err)
	}
}

// Annotate sets the Code of every session of a listing of workerID. A code
// the Worker itself sent is cleared first, on every path: only a code the Hub
// gave is ever shown.
func (c *SessionCodes) Annotate(ctx context.Context, workerID string, list []ops.SessionInfo) {
	for i := range list {
		list[i].Code = ""
	}
	if c == nil || c.Store == nil || len(list) == 0 {
		return
	}
	seen := make([]store.SessionSighting, len(list))
	for i, s := range list {
		seen[i] = store.SessionSighting{Name: s.Name, InstanceID: s.InstanceID}
	}
	w, nums, err := c.Store.SessionCodes(ctx, workerID, seen, c.now())
	if err != nil {
		c.warn("workers: session codes", err)
		return
	}
	for i := range list {
		if i < len(nums) && w > 0 && nums[i] > 0 {
			list[i].Code = ops.SessionCode{Worker: w, Session: nums[i]}.String()
		}
	}
}

// Sightings returns the codes of the given sessions of workerID, by name. A
// sighting carries the instance id the Worker reported ("" from a Worker or
// host that reports none, which is then named by its name), the same input
// [SessionCodes.Annotate] uses, so both give a session the same code.
func (c *SessionCodes) Sightings(ctx context.Context, workerID string, seen []store.SessionSighting) map[string]string {
	if c == nil || c.Store == nil || len(seen) == 0 {
		return nil
	}
	w, nums, err := c.Store.SessionCodes(ctx, workerID, seen, c.now())
	if err != nil {
		c.warn("workers: session codes", err)
		return nil
	}
	out := make(map[string]string, len(seen))
	for i, sg := range seen {
		if i < len(nums) && w > 0 && nums[i] > 0 {
			out[sg.Name] = ops.SessionCode{Worker: w, Session: nums[i]}.String()
		}
	}
	return out
}

// WorkerNumbers returns the number of every Worker, by id (nil on a failure).
func (c *SessionCodes) WorkerNumbers(ctx context.Context) map[string]int64 {
	if c == nil || c.Store == nil {
		return nil
	}
	m, err := c.Store.WorkerNumbers(ctx)
	if err != nil {
		c.warn("workers: worker numbers", err)
		return nil
	}
	return m
}

// lookup returns the Worker and the current session name of a code.
func (c *SessionCodes) lookup(ctx context.Context, code ops.SessionCode) (workerID, name string, ok bool, err error) {
	if c == nil || c.Store == nil {
		return "", "", false, nil
	}
	return c.Store.ResolveSessionCode(ctx, code.Worker, code.Session)
}
