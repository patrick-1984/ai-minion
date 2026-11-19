package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Headers of the live monitoring notice (A49 design v2, §5.7).
const (
	// HeaderMonitoring is on every API response: the policy in force.
	HeaderMonitoring = "Minion-Monitoring"
	// HeaderMonitoringSeen is sent by a client that remembers the policy
	// version it last saw.
	HeaderMonitoringSeen = "Minion-Monitoring-Seen"
	// CodeMonitoringChanged is the error code of a call refused once because
	// the monitoring policy became stricter than the client knew.
	CodeMonitoringChanged = proto.CodeMonitoringChanged
)

// setMonitoringHeader puts the policy in force on the response. Without a
// Monitor (compliance monitoring is not built in) nothing is added.
func (d *Dispatcher) setMonitoringHeader(w http.ResponseWriter) {
	if d.Monitor == nil {
		return
	}
	w.Header().Set(HeaderMonitoring, d.Monitor.Policy().Header())
}

// SeenVersion reads the Minion-Monitoring-Seen header.
func SeenVersion(r *http.Request) (int64, bool) {
	v := strings.TrimSpace(r.Header.Get(HeaderMonitoringSeen))
	if v == "" {
		return 0, false
	}
	v = strings.TrimPrefix(v, "v=")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// refuseStale answers 409 monitoring_changed and returns true when the client
// last saw a less strict policy than the one in force and op carries session
// content: nothing is routed, and the next request (which carries the new
// version) goes through.
func (d *Dispatcher) refuseStale(w http.ResponseWriter, r *http.Request, op string, kind authz.Kind) bool {
	if d.Monitor == nil {
		return false
	}
	seen, have := SeenVersion(r)
	notice, refuse, pol := d.Monitor.Stale(seen, have, op)
	if !refuse {
		return false
	}
	// One snapshot decides and answers: the header names the version the
	// notice names, so the client's retry carries the version that goes through.
	w.Header().Set(HeaderMonitoring, pol.Header())
	d.refused(w, op, "monitoring_changed", kind, NewError(proto.CodeMonitoringChanged, notice))
	return true
}

// restCaptureInfo describes a call of the REST front door for the recorder.
func restCaptureInfo(op string, args any, id Identity, res authz.Resource) compliance.CallInfo {
	info := compliance.CallInfo{
		Path: compliance.PathREST, Op: op, Args: args,
		PrincipalKind: string(id.Principal.Kind), Actor: id.Actor, ActingAs: id.ActingAs, KeyID: id.Principal.KeyID,
	}
	switch id.Principal.Kind {
	case authz.KindAPIKey:
		info.Client = compliance.ClientAPI
	default:
		info.Client = compliance.ClientBrowser
	}
	switch r := res.(type) {
	case authz.SessionResource:
		info.WorkerID = r.WorkerID
	case authz.WorkerResource:
		info.WorkerID = r.ID
	}
	return info
}

// captureStream wraps a streaming handler so that every element that reached
// the client is recorded.
func captureStream(fn StreamHandler, capt *compliance.Capture) StreamHandler {
	if capt == nil {
		return fn
	}
	return func(ctx context.Context, c *Call, emit func(any) error) error {
		err := fn(ctx, c, func(v any) error {
			if err := emit(v); err != nil {
				return err
			}
			capt.Element(v)
			return nil
		})
		code := ""
		if err != nil {
			code = string(ErrorOf(err).Code)
		}
		capt.End(code)
		return err
	}
}

// RequireFreshSession is the gate of compliance_set (A49 §5.4): the caller is
// a direct browser session (not an API key, not act-as, not a Worker) whose
// credential was checked at most [session.ReauthWindow] ago. A stale session
// gets reauth_required with the method the page should offer. The dispatcher
// has already required an administrator (the manage action on the caller's
// own user) and, for a SessionOnly op, a browser session.
func RequireFreshSession(c *Call, now time.Time) error {
	sess := c.Identity.Session
	if c.Identity.Via != ViaSession || sess == nil || sess.ActAs != nil || c.Principal.Kind != authz.KindBrowserSession {
		return NewError(proto.CodeForbidden, "this operation is available only to a signed-in browser session")
	}
	if !sess.Fresh(now) {
		e := NewError(proto.CodeReauthRequired, "Sign in again to confirm it is you; the last check is more than five minutes old.")
		e.Details = map[string]any{"method": reauthMethodKind(sess.AuthMethod)}
		return e
	}
	return nil
}
