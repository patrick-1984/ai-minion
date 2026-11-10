package api

// Story 5.3: GET /api/v1/sessions, the sessions of all Workers the caller may
// read, answered from the Hub's in-memory state (the ev streams of the
// Workers). There is no Worker round trip: the picture is as fresh as the
// Workers' last event, and a Worker that is offline keeps its last known
// sessions with online=false.
//
// Visibility has two layers. The "mine" filter (R47) is the default: only the
// Workers the caller's user owns. ?all=true lifts it, which widens the list
// for an admin and changes nothing for anyone else, because authz.Filter then
// keeps only the sessions the principal may read (AD-11: the Hub filters list
// results by scope, so a key limited to some Workers or sessions sees only
// those). Name, owner and kind of a Worker are read from the Hub's directory
// on every request, so a rename or a reassignment shows at once; only the
// session state is in memory.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// SessionsPath is the route of the aggregated session list.
const SessionsPath = "/api/v1/sessions"

// LiveState is the in-memory picture of the Workers' sessions;
// *workers.EventHub implements it.
type LiveState interface {
	// Sessions returns the latest state of one Worker.
	Sessions(workerID string) (workers.WorkerSessions, bool)
}

var _ LiveState = (*workers.EventHub)(nil)

// SessionInfo is one entry of the aggregated list and of the snapshot that
// opens an event stream.
type SessionInfo struct {
	WorkerID string `json:"worker_id"`
	// Worker is the Worker's name.
	Worker  string `json:"worker"`
	OwnerID string `json:"owner_id"`
	// Owner is the owner's user name, or the id when no name is known.
	Owner string `json:"owner"`
	// Session is the session's name.
	Session string `json:"session"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Task    string `json:"task"`
	// Online is true while the session's Worker is connected.
	Online bool `json:"online"`
	// ScreenHash identifies the last known screen, never its content.
	ScreenHash string `json:"screen_hash,omitempty"`
}

// SessionsResult is the body of GET /api/v1/sessions.
type SessionsResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionQuery narrows the list. The zero value lists the caller's own
// sessions.
type SessionQuery struct {
	// All lifts the "mine" filter.
	All bool
	// Worker keeps one Worker, by name or id.
	Worker string
	// Kind keeps a kind; "agent" also matches every "agent:<cli>".
	Kind string
	// State keeps a state.
	State string
	// Online keeps online (true) or offline (false) sessions; nil keeps both.
	Online *bool
}

// SessionLister builds the aggregated session list. It is used by the REST
// route and by the snapshot of the event stream.
type SessionLister struct {
	// Directory lists the enrolled Workers. Required.
	Directory workers.Directory
	// State is the in-memory picture. Required.
	State LiveState
	// Authz filters the list by what the principal may read. Required.
	Authz *authz.Authorizer
	// OwnerName renders an owner id as the user name; nil uses the id.
	OwnerName func(ctx context.Context, ownerID string) string
}

type sessionItem struct {
	rec workers.WorkerRecord
	st  workers.SessionState
	on  bool
}

// List returns the sessions p may read, ordered by Worker name, owner, Worker
// id and session name.
func (l *SessionLister) List(ctx context.Context, p authz.Principal, q SessionQuery) ([]SessionInfo, error) {
	recs, err := l.Directory.Workers(ctx)
	if err != nil {
		return nil, fmt.Errorf("api: read the Workers: %w", err)
	}
	var items []sessionItem
	for _, w := range recs {
		if !q.All && (p.UserID == "" || w.OwnerID != p.UserID) {
			continue
		}
		if q.Worker != "" && q.Worker != w.Name && q.Worker != w.ID {
			continue
		}
		ws, ok := l.State.Sessions(w.ID)
		if !ok {
			continue
		}
		if q.Online != nil && ws.Online != *q.Online {
			continue
		}
		for _, s := range ws.Sessions {
			if !matchClass(q.Kind, s.Kind, true) || !matchClass(q.State, s.State, false) {
				continue
			}
			items = append(items, sessionItem{rec: w, st: s, on: ws.Online})
		}
	}
	items, err = authz.Filter(ctx, l.Authz, p, items, func(it sessionItem) authz.Resource {
		return authz.SessionResource{WorkerID: it.rec.ID, Name: it.st.Name}
	})
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	out := make([]SessionInfo, 0, len(items))
	for _, it := range items {
		owner, ok := owners[it.rec.OwnerID]
		if !ok {
			owner = it.rec.OwnerID
			if l.OwnerName != nil {
				if n := l.OwnerName(ctx, it.rec.OwnerID); n != "" {
					owner = n
				}
			}
			owners[it.rec.OwnerID] = owner
		}
		out = append(out, SessionInfo{
			WorkerID: it.rec.ID, Worker: it.rec.Name, OwnerID: it.rec.OwnerID, Owner: owner,
			Session: it.st.Name, Kind: it.st.Kind, State: it.st.State, Task: it.st.Task,
			Online: it.on, ScreenHash: it.st.ScreenHash,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Worker != b.Worker:
			return a.Worker < b.Worker
		case a.Owner != b.Owner:
			return a.Owner < b.Owner
		case a.WorkerID != b.WorkerID:
			return a.WorkerID < b.WorkerID
		}
		return a.Session < b.Session
	})
	return out, nil
}

// matchClass reports whether got matches want; an empty want matches all. For
// a kind, "agent" matches "agent:<cli>" as well.
func matchClass(want, got string, kind bool) bool {
	if want == "" || want == got {
		return true
	}
	return kind && want == "agent" && strings.HasPrefix(got, ops.PaneKindAgentPrefix)
}

// Limits of the query string.
const maxQueryValueLen = 256

// ParseSessionQuery reads the filters of a sessions or events request. An
// unknown parameter is ignored (AD-4); a bad value is invalid_argument.
func ParseSessionQuery(r *http.Request) (SessionQuery, error) {
	vals := r.URL.Query()
	var q SessionQuery
	for _, f := range []struct {
		name string
		dst  *string
	}{{"worker", &q.Worker}, {"kind", &q.Kind}, {"state", &q.State}} {
		v := vals.Get(f.name)
		if len(v) > maxQueryValueLen {
			return q, invalidArg(f.name, "is too long")
		}
		*f.dst = v
	}
	if v := vals.Get("all"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return q, invalidArg("all", "must be true or false")
		}
		q.All = b
	}
	if v := vals.Get("online"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return q, invalidArg("online", "must be true or false")
		}
		q.Online = &b
	}
	return q, nil
}

// SessionsHandler serves GET /api/v1/sessions.
type SessionsHandler struct {
	// Auth authenticates the caller. Required.
	Auth Authn
	// Lister builds the list. Required.
	Lister *SessionLister
	// BodyTimeout bounds the time a client may take to send a request body
	// that it announced; zero means [DefaultBodyTimeout].
	BodyTimeout time.Duration
}

// NewSessionsHandler checks the configuration and returns the handler.
func NewSessionsHandler(auth Authn, l *SessionLister) (*SessionsHandler, error) {
	if auth == nil || l == nil || l.Directory == nil || l.State == nil || l.Authz == nil {
		return nil, fmt.Errorf("api: the sessions handler needs an authenticator and a lister with Directory, State and Authz")
	}
	return &SessionsHandler{Auth: auth, Lister: l}, nil
}

// absorbBody reads and drops the body that a GET request announces, under a
// read deadline of timeout (zero means [DefaultBodyTimeout]). The two routes
// of this file and events.go are mounted on the mux, outside the dispatcher
// and its body deadline, and the Hub's server timeouts cover the headers and
// idle connections only: without it a client that announces a body and stops
// sending would hold the connection, unauthenticated, for ever, because
// net/http reads the rest of an unread body before it writes any answer. The
// deadline is lifted once the body is read, so that none stays on a live
// event stream. When the body cannot be read in time, or is too large, the
// request is refused and the connection closed; the deadline then stays so
// that the rest of the body is not waited for either. A writer that cannot
// set a deadline (a recorder) has no connection to stall.
func absorbBody(w http.ResponseWriter, r *http.Request, timeout time.Duration) error {
	if !hasBody(r) {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultBodyTimeout
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(timeout))
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		w.Header().Set("Connection", "close")
		return invalidArg("", "the request body could not be read")
	}
	if n > MaxBodyBytes {
		w.Header().Set("Connection", "close")
		return NewError(proto.CodeMessageTooLarge, fmt.Sprintf("the request body exceeds %d bytes", MaxBodyBytes))
	}
	_ = rc.SetReadDeadline(time.Time{})
	return nil
}

// ServeHTTP implements http.Handler.
func (h *SessionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := absorbBody(w, r, h.BodyTimeout); err != nil {
		WriteError(w, err)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		WriteError(w, NewError(proto.CodeMethodNotAllowed, "use GET"))
		return
	}
	id, err := h.Auth.Authenticate(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	q, err := ParseSessionQuery(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	list, err := h.Lister.List(r.Context(), id.Principal, q)
	if err != nil {
		WriteError(w, err)
		return
	}
	if list == nil {
		list = []SessionInfo{}
	}
	body, err := json.Marshal(SessionsResult{Sessions: list})
	if err != nil {
		WriteError(w, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}
