package apikey

import (
	"slices"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Grant is one resource of a key scope: everything, a user, a Worker or one
// session of a Worker. It is the stored and the wire form alike, so a scope
// round-trips without translation.
type Grant = ops.KeyResource

// sessionKey identifies a session within the scope index.
type sessionKey struct{ workerID, name string }

// Scope is the stored scope of an API key. It implements [authz.Scope]: it
// tells which actions the key grants on a resource, and [authz.Authorizer]
// intersects that with what the key's owner currently holds (AD-11), so a
// Scope never needs to know the owner's role.
//
// A Scope is immutable and safe for concurrent use.
type Scope struct {
	everything bool
	users      map[string]struct{}
	workers    map[string]struct{}
	sessions   map[sessionKey]struct{}
	actions    []ops.Action
}

var _ authz.Scope = (*Scope)(nil)

// NewScope builds the scope that grants actions on every resource of grants.
// Unknown grant kinds and unknown actions are ignored: the stored scope is
// never wider than what it lists, and a row that no code understands
// (a later version wrote it) therefore grants nothing.
func NewScope(grants []Grant, actions []ops.Action) *Scope {
	s := &Scope{
		users:    map[string]struct{}{},
		workers:  map[string]struct{}{},
		sessions: map[sessionKey]struct{}{},
	}
	for _, g := range grants {
		switch g.Kind {
		case ops.KeyResourceEverything:
			s.everything = true
		case ops.KeyResourceUser:
			if g.UserID != "" {
				s.users[g.UserID] = struct{}{}
			}
		case ops.KeyResourceWorker:
			if g.WorkerID != "" {
				s.workers[g.WorkerID] = struct{}{}
			}
		case ops.KeyResourceSession:
			if g.WorkerID != "" && g.SessionName != "" {
				s.sessions[sessionKey{g.WorkerID, g.SessionName}] = struct{}{}
			}
		}
	}
	for _, a := range actions {
		if a.Valid() && !slices.Contains(s.actions, a) {
			s.actions = append(s.actions, a)
		}
	}
	slices.Sort(s.actions)
	return s
}

// Actions implements [authz.Scope]. The resource kinds are covered like this:
//
//   - everything covers every resource;
//   - a chosen user covers the user resource and every Worker and session that
//     user owns;
//   - a chosen Worker covers the Worker and all its sessions, and nothing else
//     of its owner;
//   - a chosen session covers exactly that session.
//
// A resource the scope does not cover returns nil. The result is a copy.
func (s *Scope) Actions(res authz.ResolvedResource) []ops.Action {
	if !s.covers(res) {
		return nil
	}
	return slices.Clone(s.actions)
}

func (s *Scope) covers(res authz.ResolvedResource) bool {
	if s.everything {
		return true
	}
	switch res.Kind {
	case authz.ResourceUser:
		_, ok := s.users[res.UserID]
		return ok
	case authz.ResourceWorker, authz.ResourceSession:
		if _, ok := s.users[res.OwnerID]; ok && res.OwnerID != "" {
			return true
		}
		if _, ok := s.workers[res.WorkerID]; ok {
			return true
		}
		if res.Kind == authz.ResourceSession {
			_, ok := s.sessions[sessionKey{res.WorkerID, res.SessionName}]
			return ok
		}
	}
	return false
}
