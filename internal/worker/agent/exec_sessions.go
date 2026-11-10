package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// maxAutoNameTries bounds how many minion-N names session_create skips
// because an adopted session already has the name.
const maxAutoNameTries = 100

// listParallel bounds the concurrent pane inspections of sessions_list.
const listParallel = 8

// pane is one resolved and classified pane.
type pane struct {
	ref    term.PaneRef
	info   ops.PaneInfo
	class  agentdetect.Result
	screen ops.Screen
	// incomplete is set when the host could not give the whole visible screen
	// (term.ErrIncomplete). The screen is the partial one, the classification
	// is made on it, and no dialog digest is issued: a binding to a screen with
	// missing content would approve changes that nobody could see.
	incomplete bool
}

// resolve resolves a target to one pane (R88: the session name is the
// identity). A missing session or pane is a target_not_found API error.
func (e *Executor) resolve(ctx context.Context, t ops.Target) (term.PaneRef, error) {
	ref, err := e.host.ResolvePane(ctx, t)
	if err != nil {
		return term.PaneRef{}, hostErr("resolve target", err)
	}
	return ref, nil
}

// basicInfo is the pane info without window, index or classification.
func basicInfo(session string, ref term.PaneRef) ops.PaneInfo {
	return ops.PaneInfo{Session: session, ID: ref.ID, Command: ref.Command, PID: ref.PID}
}

// locate fills window, index and the session generation of a pane from the
// host's listing. It is best effort: the values stay zero when the listing
// fails or the pane has just gone. A caller that binds a write to the reviewed
// pane treats a zero generation as a mismatch, so a failed listing fails safe.
func (e *Executor) locate(ctx context.Context, info *ops.PaneInfo) {
	sessions, err := e.host.ListSessions(ctx)
	if err != nil {
		return
	}
	for _, s := range sessions {
		if s.Name != info.Session {
			continue
		}
		for _, p := range s.Panes {
			if p.Ref.ID == info.ID {
				info.Window, info.Index = p.Window, p.Index
				if !s.Created.IsZero() {
					info.SessionCreated = s.Created.Unix()
				}
				return
			}
		}
	}
}

// inspect reads the visible screen of the pane and classifies it. The
// classification is the only place that decides kind and state (AD-12).
func (e *Executor) inspect(ctx context.Context, session string, ref term.PaneRef) (*pane, error) {
	scr, err := e.host.Snapshot(ctx, ref)
	incomplete := false
	if err != nil {
		if !errors.Is(err, term.ErrIncomplete) {
			return nil, hostErr("read the pane", err)
		}
		incomplete = true
	}
	p := &pane{ref: ref, screen: scr, info: basicInfo(session, ref), incomplete: incomplete}
	p.class = e.classify.Classify(ctx, agentdetect.Input{Command: ref.Command, PID: ref.PID, Screen: scr})
	p.info.Kind, p.info.State = p.class.Kind, p.class.State
	if !incomplete {
		p.info.DialogDigest = agentdetect.DialogDigest(p.class, scr)
	}
	e.locate(ctx, &p.info)
	return p, nil
}

// requireShared refuses a read of an unshared session (R75, A-9). The refusal
// names no content.
func (e *Executor) requireShared(ctx context.Context, session string) error {
	ok, err := e.policy.Shared(ctx, session)
	if err != nil {
		return internalErr("read the share selection", err)
	}
	if !ok {
		return apiErr(proto.CodeForbidden, proto.CodeNotShared, "the session is not shared")
	}
	return nil
}

// sessionsList lists the shared sessions with their active pane's kind and
// state. Unshared sessions are omitted whatever the caller holds.
func (e *Executor) sessionsList(ctx context.Context, rq Request) (any, error) {
	if _, _, err := prepare[ops.SessionsListArgs](rq); err != nil {
		return nil, err
	}
	return readStable(e, ctx, "", func(sel store.ShareState) (any, error) { return e.listShared(ctx, sel) })
}

// maxStableReads bounds how often a read is started again because the share
// rule changed while it ran.
const maxStableReads = 3

// sameShare reports whether two states are the same share choice: the same
// revision, mode and selected names.
func sameShare(a, b store.ShareState) bool {
	return a.Revision == b.Revision && a.Mode == b.Mode && slices.Equal(a.Sessions, b.Sessions)
}

// readStable runs a read of private content (a session list, a snapshot, a
// capture) so that nothing it collects after the share rule changed is ever
// delivered. The rule, with its revision, is read before the read starts and
// again after the read has collected its data and before anything is returned.
// When it differs (a share_set, or a local change, committed while the read was
// running) the data is discarded, whatever it holds, and the read is made again
// against the rule that is then in force: for a session that is no longer
// shared that is the not_shared refusal. session is the session the read is
// about; empty for a list, which read filters by the rule it is given.
//
// An error never carries data: the read returns the error and nothing else.
func readStable[T any](e *Executor, ctx context.Context, session string, read func(st store.ShareState) (T, error)) (T, error) {
	var zero T
	for range maxStableReads {
		before, err := e.policy.State(ctx)
		if err != nil {
			return zero, internalErr("read the share selection", err)
		}
		if session != "" && !before.Includes(session) {
			return zero, apiErr(proto.CodeForbidden, proto.CodeNotShared, "the session is not shared")
		}
		res, err := read(before)
		if err != nil {
			return zero, err
		}
		after, err := e.policy.State(ctx)
		if err != nil {
			return zero, internalErr("read the share selection", err)
		}
		if sameShare(before, after) {
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
	}
	return zero, apiErr(proto.CodeConflict, "", "the share rule of this Worker kept changing while the session was read; ask again")
}

// listShared lists the sessions that sel shares.
func (e *Executor) listShared(ctx context.Context, sel store.ShareState) (any, error) {
	all, err := e.host.ListSessions(ctx)
	if err != nil {
		return nil, hostErr("list sessions", err)
	}
	var shared []term.Session
	for _, s := range all {
		if sel.Includes(s.Name) {
			shared = append(shared, s)
		}
	}
	out := make([]ops.SessionInfo, len(shared))
	sem := make(chan struct{}, listParallel)
	var wg sync.WaitGroup
	for i, s := range shared {
		out[i] = ops.SessionInfo{
			Name: s.Name, Created: ops.Timestamp(s.Created), Windows: s.Windows,
			Attached: s.Attached, Task: s.Task, Probe: s.Probe,
		}
		p, ok := s.ActivePane()
		if !ok {
			continue
		}
		info := basicInfo(s.Name, p.Ref)
		info.Window, info.Index = p.Window, p.Index
		info.Kind, info.State = ops.PaneKindUnknown, ops.PaneStateUnknown
		out[i].Pane = &info
		wg.Add(1)
		sem <- struct{}{}
		go func(info *ops.PaneInfo, ref term.PaneRef) {
			defer wg.Done()
			defer func() { <-sem }()
			// A pane that cannot be read keeps kind and state unknown.
			if scr, err := e.host.Snapshot(ctx, ref); err == nil {
				r := e.classify.Classify(ctx, agentdetect.Input{Command: ref.Command, PID: ref.PID, Screen: scr})
				info.Kind, info.State = r.Kind, r.State
			}
		}(&info, p.Ref)
	}
	wg.Wait()
	return ops.SessionsListResult{Sessions: out}, nil
}

// readTarget is the shared start of snapshot and capture: share check,
// resolution, screen and classification.
func (e *Executor) readTarget(ctx context.Context, t ops.Target) (*pane, error) {
	if err := e.requireShared(ctx, t.SessionName); err != nil {
		return nil, err
	}
	ref, err := e.resolve(ctx, t)
	if err != nil {
		return nil, err
	}
	return e.inspect(ctx, t.SessionName, ref)
}

func (e *Executor) sessionSnapshot(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.SessionSnapshotArgs](rq)
	if err != nil {
		return nil, err
	}
	return readStable(e, ctx, args.Target.SessionName, func(store.ShareState) (any, error) {
		p, err := e.readTarget(ctx, args.Target)
		if err != nil {
			return nil, err
		}
		return ops.SessionSnapshotResult{Pane: p.info, Screen: p.screen}, nil
	})
}

func (e *Executor) sessionCapture(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.SessionCaptureArgs](rq)
	if err != nil {
		return nil, err
	}
	return readStable(e, ctx, args.Target.SessionName, func(store.ShareState) (any, error) {
		p, err := e.readTarget(ctx, args.Target)
		if err != nil {
			return nil, err
		}
		scr, err := e.host.Capture(ctx, p.ref, args.EffectiveLines())
		if err != nil {
			return nil, hostErr("capture the pane", err)
		}
		// The review metadata and the content must be one observation. The digest
		// is the one of the visible screen that p was classified on; the capture is a
		// second read. The caller may bind a write to it only when the capture shows
		// that very screen as its last rows, otherwise it is not what the digest is
		// about, and no digest is given (the caller takes a snapshot, which is one
		// observation by construction).
		info := p.info
		if info.DialogDigest != "" && !captureShowsScreen(scr, p.screen) {
			info.DialogDigest = ""
		}
		return ops.SessionCaptureResult{Pane: info, Screen: scr}, nil
	})
}

// captureShowsScreen reports whether capture holds the visible screen visible
// as its last rows, row for row, ignoring only the blank rows at the bottom of
// both (a capture ends with the last non-blank row). A capture of fewer rows
// than the visible screen needs cannot show it.
func captureShowsScreen(capture, visible ops.Screen) bool {
	want := trimBottomBlank(visible.Lines)
	got := trimBottomBlank(capture.Lines)
	if len(want) == 0 || len(got) < len(want) {
		return false
	}
	tail := got[len(got)-len(want):]
	for i := range want {
		if want[i] != tail[i] {
			return false
		}
	}
	return true
}

func trimBottomBlank(lines []string) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// sessionCreate creates a detached session. Without a name it takes the next
// number of the persisted counter (minion-N, never reused, R74); a name that
// is taken is a conflict and the existing session is never touched (R86).
func (e *Executor) sessionCreate(ctx context.Context, rq Request) (any, error) {
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.SessionCreateArgs) (ops.WriteArgs, ops.Target) {
		return a.WriteArgs, ops.Target{SessionName: a.Name}
	})
	if err != nil {
		return nil, err
	}
	return e.write(ctx, op, args.WriteArgs, ops.Target{SessionName: args.Name}, false,
		func(ctx context.Context) (ops.WriteResult, error) {
			cwd := args.Cwd
			if cwd == "" {
				cwd = e.cwd
			}
			sess, err := e.createSession(ctx, args.Name, cwd, args.Argv)
			var amb *ambiguousCreate
			if errors.As(err, &amb) {
				// The program may have started: report unknown and let the
				// request id keep the answer (AD-15).
				return ops.WriteResult{
					Outcome: ops.OutcomeUnknown, Reason: proto.CodeInternal.String(),
					Pane: &ops.PaneInfo{Session: amb.name},
				}, nil
			}
			if err != nil {
				return ops.WriteResult{}, err
			}
			res := ops.WriteResult{Outcome: ops.OutcomeDelivered}
			if p, ok := sess.ActivePane(); ok {
				info := basicInfo(sess.Name, p.Ref)
				info.Window, info.Index = p.Window, p.Index
				r := e.classify.Classify(ctx, agentdetect.Input{Command: p.Ref.Command, PID: p.Ref.PID})
				info.Kind, info.State = r.Kind, r.State
				res.Pane = &info
			}
			return res, nil
		})
}

func (e *Executor) createSession(ctx context.Context, name, cwd string, argv []string) (term.Session, error) {
	auto := name == ""
	for try := 0; try < maxAutoNameTries; try++ {
		if auto {
			n, err := e.state.NextSessionNumberContext(ctx)
			if err != nil {
				return term.Session{}, internalErr("take the next session number", err)
			}
			name = store.SessionName(n)
		}
		exists, err := e.sessionExists(ctx, name)
		if err != nil {
			return term.Session{}, hostErr("list sessions", err)
		}
		if !exists {
			sess, err := e.host.CreateSession(ctx, name, cwd, argv)
			if err == nil {
				return sess, nil
			}
			switch {
			case errors.Is(err, ops.ErrInvalidArgument):
				return term.Session{}, hostErr("create the session", err)
			case nameTaken(err, name):
				// Another creator took the name since the check, and the
				// adapter guarantees nothing was started.
			default:
				// Any other failure may come after the session was created
				// and its program started, and the program may even have
				// exited already (a timeout, a failed readback, a program that
				// is gone before the readback). A session that is not there
				// proves nothing, so only an error that says nothing was
				// started (above) is a plain failure. The result is unknown
				// and the request id keeps it: a repeat must not start the
				// program again, and retrying under another name would.
				e.log.Warn("create session failed, the session may have been created", "err", err)
				return term.Session{}, &ambiguousCreate{name: name}
			}
		}
		if !auto {
			return term.Session{}, apiErr(proto.CodeConflict, "", "a session with this name already exists")
		}
	}
	return term.Session{}, apiErr(proto.CodeConflict, "", "no free minion-N name was found")
}

// ambiguousCreate is returned by createSession when it cannot tell whether the
// session was created. It never leaves the package.
type ambiguousCreate struct{ name string }

func (a *ambiguousCreate) Error() string { return "agent: the session may have been created" }

// nameTaken reports whether a CreateSession error says that the name was
// taken, which both adapters report before they start anything. The tmux
// adapter wraps tmux.ErrSessionExists; the ConPTY host returns the exact text
// "conpty: session <name> already exists" (a sentinel would live in another
// story's package). Text matching must never be looser than that: an error
// that merely mentions "already exists" (a session whose own name says so, a
// failed readback after its program ran) is a post-launch failure, and
// treating it as a name conflict would let the same request id start the
// program twice.
func nameTaken(err error, name string) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, tmux.ErrSessionExists) || err.Error() == fmt.Sprintf("conpty: session %q already exists", name)
}

func (e *Executor) sessionExists(ctx context.Context, name string) (bool, error) {
	all, err := e.host.ListSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range all {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}
