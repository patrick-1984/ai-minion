package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// The poll interval setting (Story 5.2): how often the Worker looks at its
// shared sessions for changes to publish on the event stream.
const (
	// KeySessionPollInterval is the setting key. Flag --session-poll-interval,
	// environment MINION_SESSION_POLL_INTERVAL.
	KeySessionPollInterval = "session_poll_interval"
	// DefaultPollInterval is the default value of the setting.
	DefaultPollInterval = time.Second
	// MinPollInterval and MaxPollInterval bound the setting: a faster poll
	// would load tmux for no benefit, a slower one makes the Hub's view
	// stale.
	MinPollInterval = 100 * time.Millisecond
	MaxPollInterval = time.Minute
)

func init() {
	config.Declare(config.Setting{
		Key:     KeySessionPollInterval,
		Scope:   config.ScopeWorker,
		Default: DefaultPollInterval.String(),
		Help: "How often the Worker looks for changes in its shared sessions to report to the Hub, " +
			"as a duration between 100ms and 1m, for example 500ms",
	})
}

// ResolvePollInterval resolves and validates the poll interval with res. A nil
// resolver gives the default.
func ResolvePollInterval(res *config.Resolver) (time.Duration, error) {
	if res == nil {
		return DefaultPollInterval, nil
	}
	v, err := res.Resolve(KeySessionPollInterval)
	if err != nil {
		return 0, err
	}
	return ParsePollInterval(v.Value)
}

// ParsePollInterval parses a poll interval and checks its bounds.
func ParsePollInterval(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("setting %q: %q is not a duration such as 500ms or 2s", KeySessionPollInterval, v)
	}
	if d < MinPollInterval || d > MaxPollInterval {
		return 0, fmt.Errorf("setting %q: %s is outside %s to %s", KeySessionPollInterval, d, MinPollInterval, MaxPollInterval)
	}
	return d, nil
}

// SessionState is what the Hub learns about one shared session: the kind and
// state of its active pane (AD-12, closed sets), the session's task (the text
// that set_task stored, "" when unset) and a hash of the sanitised screen. It
// never holds screen content. Task is always on the wire, as "" when unset,
// so that a cleared task is never mistaken for a task that was not reported.
type SessionState struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Task       string `json:"task"`
	ScreenHash string `json:"screen_hash,omitempty"`
}

// View is the shared sessions at one moment, by session name.
type View map[string]SessionState

// Observer reads the current View. Unshared sessions are not in it: that is
// the one place where the share selection meets the event stream, so no event
// can name an unshared session (R75, A-9).
type Observer interface {
	Observe(ctx context.Context) (View, error)
}

// Names of the events on the event stream.
const (
	EventSessionAdded   = "session_added"
	EventSessionRemoved = "session_removed"
	EventStateChanged   = "state_changed"
	EventScreenChanged  = "screen_changed"
)

// Change is one difference between two views.
type Change struct {
	Event   string
	Session SessionState
}

// Diff returns what changed from prev to cur, ordered by session name. A
// session that is only in cur is added, one that is only in prev is removed
// (the Change then carries just the name). For a session in both, a change of
// kind, state or task is state_changed and, independently, a change of the screen
// hash is screen_changed.
func Diff(prev, cur View) []Change {
	names := make([]string, 0, len(prev)+len(cur))
	for n := range cur {
		names = append(names, n)
	}
	for n := range prev {
		if _, ok := cur[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []Change
	for _, n := range names {
		c, inCur := cur[n]
		p, inPrev := prev[n]
		switch {
		case inCur && !inPrev:
			out = append(out, Change{EventSessionAdded, c})
		case !inCur && inPrev:
			out = append(out, Change{EventSessionRemoved, SessionState{Name: n}})
		default:
			if c.Kind != p.Kind || c.State != p.State || c.Task != p.Task {
				out = append(out, Change{EventStateChanged, c})
			}
			if c.ScreenHash != p.ScreenHash {
				out = append(out, Change{EventScreenChanged, c})
			}
		}
	}
	return out
}

// ScreenHash is the hash of a sanitised screen: the text lines, the style
// spans and the size, not the cursor (a blinking cursor is not a change).
// Equal screens have equal hashes. It is hex, 32 characters.
func ScreenHash(s ops.Screen) string {
	b, _ := json.Marshal(struct {
		W, H  int
		Lines []string
		Spans []ops.StyleSpan
	}{s.Width, s.Height, s.Lines, s.Spans})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// HostObserverConfig configures a [HostObserver]. All fields are required.
type HostObserverConfig struct {
	Host       term.SessionHost
	Classifier Classifier
	Policy     *Policy
}

// HostObserver is the production [Observer]: it lists the sessions of the
// terminal host, keeps the shared ones, and reads and classifies the active
// pane of each. Its pane reads are the same snapshot and classification that
// sessions_list uses, so the Hub's two views agree.
type HostObserver struct {
	cfg HostObserverConfig
}

// NewHostObserver validates cfg and returns the observer.
func NewHostObserver(cfg HostObserverConfig) (*HostObserver, error) {
	if cfg.Host == nil || cfg.Classifier == nil || cfg.Policy == nil {
		return nil, errors.New("agent: the host observer needs a host, a classifier and a policy")
	}
	return &HostObserver{cfg: cfg}, nil
}

// Observe implements [Observer]. A pane that cannot be read keeps kind and
// state unknown and has no screen hash, like in sessions_list.
func (o *HostObserver) Observe(ctx context.Context) (View, error) {
	v, _, err := o.ObserveShare(ctx)
	return v, err
}

// shareDeclOf is the declaration of a share state: the rule with its revision.
func shareDeclOf(st store.ShareState) ops.ShareDecl {
	d := ops.ShareDecl{Rule: ops.ShareRule{Mode: string(st.Mode)}, Revision: st.Revision}
	if st.Mode == store.ShareSelect {
		d.Rule.Names = slices.Clone(st.Sessions)
		slices.Sort(d.Rule.Names)
		d.Rule.Names = slices.Compact(d.Rule.Names)
	}
	return d
}

// ShareDecl implements [ShareObserver]: the share state alone.
func (o *HostObserver) ShareDecl(ctx context.Context) (ops.ShareDecl, error) {
	st, err := o.cfg.Policy.State(ctx)
	if err != nil {
		return ops.ShareDecl{}, fmt.Errorf("read the share selection: %w", err)
	}
	return shareDeclOf(st), nil
}

// ObserveShare implements [ShareObserver]. The declaration is the state the
// view was filtered by, read after the panes were looked at (see below), so the
// view never holds a session that the declared rule does not share.
func (o *HostObserver) ObserveShare(ctx context.Context) (View, ops.ShareDecl, error) {
	for range maxStableReads {
		v, before, after, err := o.observeOnce(ctx)
		if err != nil {
			return nil, ops.ShareDecl{}, err
		}
		if sameShare(before, after) {
			return v, shareDeclOf(after), nil
		}
		// The rule changed while the panes were read: what was collected may
		// have been read after a session stopped being shared. Look again.
	}
	return nil, ops.ShareDecl{}, errors.New("the share rule kept changing during the look")
}

// observeOnce is one look: the view, the share state before it and the one
// after it, by which the view is filtered.
func (o *HostObserver) observeOnce(ctx context.Context) (View, store.ShareState, store.ShareState, error) {
	sel, err := o.cfg.Policy.State(ctx)
	if err != nil {
		return nil, sel, sel, fmt.Errorf("read the share selection: %w", err)
	}
	before := sel
	all, err := o.cfg.Host.ListSessions(ctx)
	if err != nil {
		return nil, before, sel, fmt.Errorf("list sessions: %w", err)
	}
	var shared []term.Session
	for _, s := range all {
		if sel.Includes(s.Name) {
			shared = append(shared, s)
		}
	}
	states := make([]SessionState, len(shared))
	sem := make(chan struct{}, listParallel)
	var wg sync.WaitGroup
	for i, s := range shared {
		states[i] = SessionState{Name: s.Name, Kind: ops.PaneKindUnknown, State: ops.PaneStateUnknown, Task: s.Task}
		p, ok := s.ActivePane()
		if !ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(st *SessionState, ref term.PaneRef) {
			defer wg.Done()
			defer func() { <-sem }()
			scr, err := o.cfg.Host.Snapshot(ctx, ref)
			if err != nil {
				return
			}
			r := o.cfg.Classifier.Classify(ctx, agentdetect.Input{Command: ref.Command, PID: ref.PID, Screen: scr})
			st.Kind, st.State, st.ScreenHash = r.Kind, r.State, ScreenHash(scr)
		}(&states[i], p.Ref)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, before, sel, err
	}
	// The share selection may have been withdrawn while the panes were read:
	// look again and drop what is no longer shared, so that no newly private
	// state or screen hash is published (AC 3). What is shared a moment after
	// this check is a later poll's concern; the stream never shows more than
	// the selection of the moment it was checked.
	sel, err = o.cfg.Policy.State(ctx)
	if err != nil {
		return nil, before, sel, fmt.Errorf("read the share selection: %w", err)
	}
	v := make(View, len(states))
	for _, st := range states {
		if sel.Includes(st.Name) {
			v[st.Name] = st
		}
	}
	return v, before, sel, nil
}

// Sorted returns the states of the view ordered by session name.
func (v View) Sorted() []SessionState {
	out := make([]SessionState, 0, len(v))
	for _, s := range v {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
