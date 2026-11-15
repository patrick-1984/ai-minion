package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// SessionControlStore is what the session-control handlers need;
// *store.Store implements it.
type SessionControlStore interface {
	ListSessionControl(ctx context.Context) ([]store.SessionControlRow, error)
	PutSessionControl(ctx context.Context, r store.SessionControlRow) error
	DeleteSessionControl(ctx context.Context, workerID, sessionName string) (bool, error)
}

var _ SessionControlStore = (*store.Store)(nil)

// RegisterSessionControl registers session_control_list and
// session_control_set on r. Both need manage, which only admins hold:
// session_control_list is decided on the caller's own user resource,
// session_control_set on the controller session it names, so an unknown
// Worker answers not_found. Grants are Hub-wide, so an API key whose scope
// does not reach the whole Hub is refused. The dispatcher writes the audit
// record of session_control_set, with the controller session as its target.
func RegisterSessionControl(r *Registry, st SessionControlStore) error {
	if st == nil {
		return errors.New("api: the session-control handlers need a store")
	}
	h := sessionControlHandlers{st: st}
	if err := HandleTyped(r, ops.OpSessionControlList, h.list); err != nil {
		return err
	}
	return HandleTyped(r, ops.OpSessionControlSet, h.set)
}

type sessionControlHandlers struct{ st SessionControlStore }

func (h sessionControlHandlers) list(ctx context.Context, c *Call, _ ops.SessionControlListArgs) (ops.SessionControlListResult, error) {
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.SessionControlListResult{}, err
	}
	rows, err := h.st.ListSessionControl(ctx)
	if err != nil {
		return ops.SessionControlListResult{}, err
	}
	out := ops.SessionControlListResult{Grants: make([]ops.SessionControlInfo, 0, len(rows))}
	for _, row := range rows {
		var targets []ops.KeyResource
		if err := json.Unmarshal([]byte(row.TargetsJSON), &targets); err != nil {
			return ops.SessionControlListResult{}, fmt.Errorf("api: session control of %s:%s: %w", row.WorkerID, row.SessionName, err)
		}
		out.Grants = append(out.Grants, ops.SessionControlInfo{
			WorkerID: row.WorkerID, SessionName: row.SessionName, Targets: targets,
			CreatedBy: row.CreatedBy, CreatedAt: ops.Timestamp(store.FromUnixMS(row.CreatedAt)),
		})
	}
	return out, nil
}

func (h sessionControlHandlers) set(ctx context.Context, c *Call, a ops.SessionControlSetArgs) (ops.WriteResult, error) {
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.WriteResult{}, err
	}
	if len(a.Targets) == 0 {
		if _, err := h.st.DeleteSessionControl(ctx, a.WorkerID, a.SessionName); err != nil {
			return ops.WriteResult{}, err
		}
		return delivered(), nil
	}
	targets, err := json.Marshal(a.Targets)
	if err != nil {
		return ops.WriteResult{}, err
	}
	err = h.st.PutSessionControl(ctx, store.SessionControlRow{
		WorkerID: a.WorkerID, SessionName: a.SessionName, TargetsJSON: string(targets),
		CreatedBy: c.Identity.Actor, CreatedAt: store.UnixMS(time.Now()),
	})
	if err != nil {
		return ops.WriteResult{}, err
	}
	return delivered(), nil
}
