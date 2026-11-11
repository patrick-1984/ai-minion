package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// taskOption is the session option that holds the task text (set_task).
const taskOption = "@minion_task"

// auditTimeout bounds the bookkeeping that runs after a write, on a context
// that survives the caller's cancellation: input that went out is recorded
// even when the caller has gone.
const auditTimeout = 5 * time.Second

// warnReadiness is the WARN-class finding for a pane whose state could not be
// told (guard readiness_unknown). It never blocks.
var warnReadiness = ops.Warning{
	Code:    "readiness_unknown",
	Message: "the state of the pane could not be told; the input was sent anyway",
}

// write is the frame of every write op (AD-15): local policy, one request at
// a time per request id, the duplicate cache, the op itself, then the cache
// entry and the audit row.
//
// Order matters. Policy refusals and permission failures come first and are
// API errors, never outcomes (and never cached). A duplicate request id is
// then answered from the cache without touching the pane. The cache covers
// every outcome, a refusal too: a caller that wants to repeat a refused write
// with force=true makes a new request.
func (e *Executor) write(ctx context.Context, op ops.Op, wa ops.WriteArgs, t ops.Target, checkShared bool,
	run func(ctx context.Context) (ops.WriteResult, error)) (_ ops.WriteResult, err error) {

	var res ops.WriteResult
	cached := false
	defer func() {
		if !cached {
			e.record(ctx, op.Name, wa, t, res, err)
		}
	}()

	if err = e.checkPolicy(ctx, t, checkShared); err != nil {
		return res, err
	}

	unlock, lerr := e.locks.Lock(ctx, requestKey(wa.RequestID), e.lockWait)
	if lerr != nil {
		// The request with this id is still running and may yet deliver, so a
		// duplicate must never answer failed_not_sent: that invites a resend
		// and a double delivery (AD-15). The answer is unknown, it is not
		// cached, and it reaches the audit through res.
		if errors.Is(lerr, ErrLockTimeout) {
			res = ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: proto.CodeBusy.String()}
			return res, nil
		}
		return res, lerr
	}
	defer unlock()

	// The policy may have changed while the request waited for its lock.
	if err = e.checkPolicy(ctx, t, checkShared); err != nil {
		return res, err
	}

	rec, hit := e.memResult(wa.RequestID)
	if !hit {
		var gerr error
		rec, hit, gerr = e.state.GetResult(ctx, wa.RequestID)
		if gerr != nil {
			return res, internalErr("read the result cache", gerr)
		}
	}
	if hit {
		if rec.Op != op.Name || rec.Session != t.SessionName || rec.Force != wa.Force {
			return res, apiErr(proto.CodeConflict, "", "this request_id was already used for a different request")
		}
		if jerr := json.Unmarshal(rec.Result, &res); jerr != nil {
			return res, internalErr("decode the cached result", jerr)
		}
		cached = true
		return res, nil
	}

	// Record the request before it has any effect. If the Worker dies or the
	// result cannot be stored afterwards, a duplicate finds this record and
	// answers unknown instead of sending again (AD-15). Without the record the
	// request does not run.
	rec = store.ResultRecord{RequestID: wa.RequestID, Op: op.Name, Session: t.SessionName, Force: wa.Force, Result: []byte(unfinishedResult)}
	if perr := e.state.PutResult(ctx, rec); perr != nil {
		return res, internalErr("record the request", perr)
	}

	res, err = run(ctx)
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err != nil {
		// An API error means nothing was sent: the caller may repeat the
		// request. If the record cannot be removed, a repeat answers unknown.
		if derr := e.state.DeleteResult(bctx, wa.RequestID); derr != nil {
			e.log.Warn("cannot remove the record of a failed write", "op", op.Name, "err", derr)
		}
		return res, err
	}
	if verr := res.Validate(); verr != nil {
		// A bug in the executor, not a caller error; the input may have gone,
		// so the unfinished record stays and a duplicate answers unknown.
		e.log.Error("write result violates the action contract", "op", op.Name, "err", verr)
		return ops.WriteResult{}, internalErr("build the result", verr)
	}
	b, merr := json.Marshal(res)
	if merr != nil {
		// Cannot happen for a validated result; the input went out, so the
		// duplicate must still not repeat it.
		e.log.Warn("cannot encode a write result for the cache", "op", op.Name, "err", merr)
		return res, nil
	}
	rec.Result = b
	if perr := e.state.ReplaceResult(bctx, rec); perr != nil {
		// The input went out. The database still holds the unfinished record,
		// which answers unknown after a restart; the memory entry keeps the
		// precise answer for duplicates of this process.
		e.log.Warn("cannot store a write result in the database, keeping it in memory", "op", op.Name, "err", perr)
		e.memPut(rec)
	}
	return res, nil
}

// checkPolicy applies the local policy: a preview-only Worker and a session
// that is not shared accept no write. It is checked before a write waits for
// its locks and again after every lock it took, right before the mutation, so
// a policy change during the wait takes effect.
func (e *Executor) checkPolicy(ctx context.Context, t ops.Target, checkShared bool) error {
	if e.policy.PreviewOnly() {
		return apiErr(proto.CodeForbidden, proto.CodePreviewOnly, "the Worker is preview-only")
	}
	if checkShared {
		ok, err := e.policy.Shared(ctx, t.SessionName)
		if err != nil {
			return internalErr("read the share selection", err)
		}
		if !ok {
			// A session the Worker does not share is not writable, as a
			// preview-only one is not.
			return apiErr(proto.CodeForbidden, proto.CodePreviewOnly, "the session is not shared")
		}
	}
	return nil
}

// unfinishedResult is the result a duplicate gets for a request that was
// recorded and never completed: the input may or may not have gone out.
const unfinishedResult = `{"outcome":"unknown","reason":"internal"}`

// memResult returns the in-memory entry of a request id, if it has not
// expired. It covers completed results the database could not store.
func (e *Executor) memResult(requestID string) (store.ResultRecord, bool) {
	e.memMu.Lock()
	defer e.memMu.Unlock()
	m, ok := e.mem[requestID]
	if !ok || time.Since(m.at) >= store.ResultTTL {
		delete(e.mem, requestID)
		return store.ResultRecord{}, false
	}
	return m.rec, true
}

func (e *Executor) memPut(rec store.ResultRecord) {
	e.memMu.Lock()
	defer e.memMu.Unlock()
	if e.mem == nil {
		e.mem = map[string]memEntry{}
	}
	now := time.Now()
	for id, m := range e.mem {
		if now.Sub(m.at) >= store.ResultTTL {
			delete(e.mem, id)
		}
	}
	e.mem[rec.RequestID] = memEntry{rec: rec, at: now}
}

type memEntry struct {
	rec store.ResultRecord
	at  time.Time
}

// lockFailure turns a failed lock wait into the KEEP outcome busy; a context
// that ended is returned as is.
func (e *Executor) lockFailure(err error, info *ops.PaneInfo) (ops.WriteResult, error) {
	if errors.Is(err, ErrLockTimeout) {
		return ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: proto.CodeBusy.String(), Pane: info}, nil
	}
	return ops.WriteResult{}, err
}

// record writes the local audit row of a write: op, target, outcome, request
// id, time, and never content (AD-20).
func (e *Executor) record(ctx context.Context, op string, wa ops.WriteArgs, t ops.Target, res ops.WriteResult, err error) {
	rec := store.AuditRecord{Op: op, Outcome: string(res.Outcome), Reason: res.Reason, Forced: res.Forced, RequestID: wa.RequestID, Origin: auditOrigin(ctx)}
	switch {
	case t.SessionName != "":
		rec.Target = t.String()
	case res.Pane != nil && res.Pane.Session != "":
		// session_create without a name: the session that was created.
		rec.Target = ops.Target{SessionName: res.Pane.Session}.String()
	}
	if err != nil {
		rec.Outcome, rec.Reason, rec.Forced = "error", "internal", false
		if ae, ok := AsAPIError(err); ok {
			rec.Reason = string(ae.Code)
			if ae.Reason != "" {
				rec.Reason = string(ae.Reason)
			}
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			rec.Reason = "canceled"
		}
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if aerr := e.state.AppendAudit(bctx, rec); aerr != nil {
		e.log.Warn("cannot write the local audit", "op", op, "err", aerr)
	}
}

// guardFunc lists the FORCE guards that fire for a pane, as reason codes in
// the order they are reported.
type guardFunc func(p *pane) []proto.Code

// sendFunc performs the write into the classified pane and reports the
// outcome and, when it is not delivered, the reason. It is called only when
// no guard blocks. An error ends the call as an API error before anything
// was sent.
type sendFunc func(ctx context.Context, p *pane) (ops.Outcome, proto.Code, error)

// paneWrite runs a write that lands in a pane (send_text, send_keys,
// answer): writer lock, fresh resolution and classification, the message
// versus operate check, the FORCE guards and the write itself.
func (e *Executor) paneWrite(ctx context.Context, rq Request, op ops.Op, wa ops.WriteArgs, t ops.Target,
	warnUnknown bool, guards guardFunc, send sendFunc) (ops.WriteResult, error) {

	return e.write(ctx, op, wa, t, true, func(ctx context.Context) (ops.WriteResult, error) {
		return e.paneRun(ctx, rq, op, wa, t, warnUnknown, guards, send)
	})
}

// paneRun is the body of [Executor.paneWrite] without the write frame (policy,
// request id, cache, audit): writer lock, fresh resolution and classification,
// the message versus operate check, the FORCE guards and the write itself. A
// caller that needs work of its own inside the frame, the Solo pipe's broker
// (Story 7.3), runs paneRun from its own [Executor.write] call.
func (e *Executor) paneRun(ctx context.Context, rq Request, op ops.Op, wa ops.WriteArgs, t ops.Target,
	warnUnknown bool, guards guardFunc, send sendFunc) (ops.WriteResult, error) {

	ref, err := e.resolve(ctx, t)
	if err != nil {
		return ops.WriteResult{}, err
	}
	info := basicInfo(t.SessionName, ref)
	unlock, lerr := e.locks.Lock(ctx, PaneKey(ref.ID), e.lockWait)
	if lerr != nil {
		return e.lockFailure(lerr, &info)
	}
	defer unlock()
	if err := e.checkPolicy(ctx, t, true); err != nil {
		return ops.WriteResult{}, err
	}

	// The pane may have changed while the request waited for the lock.
	fresh, err := e.resolve(ctx, t)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if !fresh.Same(ref) {
		info = basicInfo(t.SessionName, fresh)
		return ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: proto.CodePaneChanged.String(), Pane: &info}, nil
	}
	p, err := e.inspect(ctx, t.SessionName, fresh)
	if err != nil {
		return ops.WriteResult{}, err
	}
	// Permission is an API error, decided once the kind is known.
	if err := authorizeWrite(op.Actions, rq.Actions, p.info.Kind); err != nil {
		return ops.WriteResult{}, err
	}

	res := ops.WriteResult{Pane: &p.info}
	if warnUnknown && p.info.State == ops.PaneStateUnknown {
		res.Warnings = append(res.Warnings, warnReadiness)
	}
	if hits := guards(p); len(hits) > 0 {
		if !wa.Force {
			r := ops.Refused(hits[0].String(), &p.info, res.Warnings...)
			return r, nil
		}
		res.Forced = true
	}
	outcome, reason, err := send(ctx, p)
	if err != nil {
		return ops.WriteResult{}, err
	}
	res.Outcome = outcome
	if outcome != ops.OutcomeDelivered {
		res.Reason = reason.String()
	}
	return res, nil
}

// inputGuards are the FORCE guards of typed input (send_text, send_keys): a
// shell pane, an open dialog and a busy agent each refuse unless force is
// set (R87).
func inputGuards(p *pane) []proto.Code {
	var hits []proto.Code
	if p.info.Kind == ops.PaneKindShell {
		hits = append(hits, proto.CodeShellPane)
	}
	if p.info.State == ops.PaneStateDialog {
		hits = append(hits, proto.CodeDialogOpen)
	}
	if p.info.State == ops.PaneStateBusy {
		hits = append(hits, proto.CodeBusy)
	}
	return hits
}

// failureOf maps an error of the host's write to an outcome and a reason.
// sent is the number of keys that went out before it. A pane change and a
// vanished pane are KEEP failures; once anything was sent the outcome is
// partial, never failed_not_sent (AD-15). Any other error (an ended context, a
// timed-out or broken client call) cannot tell whether the key in flight went
// through: the adapter counts a key only after its call returned cleanly. So
// the result is partial when keys are known to have gone out and unknown when
// none is; failed_not_sent is reserved for errors that establish no delivery.
func failureOf(err error, sent int) (ops.Outcome, proto.Code) {
	switch {
	case errors.Is(err, term.ErrPaneChanged):
		return ops.FailureOutcome(sent > 0), proto.CodePaneChanged
	case errors.Is(err, term.ErrNotFound):
		return ops.FailureOutcome(sent > 0), proto.CodeTargetNotFound
	}
	if sent > 0 {
		return ops.OutcomePartial, proto.CodeInternal
	}
	return ops.OutcomeUnknown, proto.CodeInternal
}

func (e *Executor) sendText(ctx context.Context, rq Request) (any, error) {
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.SendTextArgs) (ops.WriteArgs, ops.Target) { return a.WriteArgs, a.Target })
	if err != nil {
		return nil, err
	}
	return e.paneWrite(ctx, rq, op, args.WriteArgs, args.Target, true, inputGuards,
		func(ctx context.Context, p *pane) (ops.Outcome, proto.Code, error) {
			return e.pasteAndSubmit(ctx, p, args.Text, args.Submit)
		})
}

// pasteAndSubmit pastes the text into the classified pane and, when submit is
// set, presses Enter there. It is [Executor.pasteSubmit] with the plain paste of
// the host; the pipe delivery runs the same primitive with the marked paste.
func (e *Executor) pasteAndSubmit(ctx context.Context, p *pane, text string, submit bool) (ops.Outcome, proto.Code, error) {
	return e.pasteSubmit(ctx, p, "text", submit, func(ctx context.Context) error {
		return e.host.PasteText(ctx, p.ref, text, p.ref)
	})
}

// pasteSubmit is the one guarded delivery of typed text into a pane: the paste
// (the closure, which must use p.ref as both pane and expected identity) and,
// when submit is set, the Enter. It is shared by send_text and the pipe
// delivery, so that both have the same identity checks and the same truthful
// outcomes, and a change to one cannot leave the other behind.
//
// It runs inside paneRun, so the paste and the Enter share one request id, one
// resolved pane and one pane lock, and both steps are checked against the same
// expected identity (p.ref): the host compares pane id, pid and command before
// the paste and again before the Enter, and sends nothing when the pane is no
// longer that one (KEEP). The caller has checked the policy and the share under
// that lock already (paneRun); nothing here mutates before that.
//
// Truthful outcomes (AD-15). Before the paste nothing is sent, so a refusal of
// the paste (the pane changed, an invalid argument) may be failed_not_sent. A
// paste is one step with no count: after any other error nobody can tell
// whether the text arrived, so the result is unknown and no Enter follows. Once
// the paste went out, nothing is failed_not_sent any more: an Enter that was
// refused (the pane changed or vanished) is partial, because the text is in the
// pane without its submission; an Enter whose outcome is unknown is unknown.
// what names the delivery in the log and in an argument error.
func (e *Executor) pasteSubmit(ctx context.Context, p *pane, what string, submit bool,
	paste func(ctx context.Context) error) (ops.Outcome, proto.Code, error) {

	if err := paste(ctx); err != nil {
		e.log.Warn("the paste failed", "what", what, "err", err)
		if errors.Is(err, ops.ErrInvalidArgument) {
			// The adapter refused the argument before sending anything.
			return "", "", hostErr("paste the "+what, err)
		}
		if errors.Is(err, term.ErrPaneChanged) || errors.Is(err, term.ErrNotFound) {
			o, r := failureOf(err, 0)
			return o, r, nil
		}
		// A paste is one step with no count: after any other error nobody
		// can tell whether the text arrived.
		return ops.OutcomeUnknown, proto.CodeInternal, nil
	}
	if !submit {
		return ops.OutcomeDelivered, "", nil
	}
	sent, err := e.host.SendKeys(ctx, p.ref, []string{"Enter"}, p.ref)
	if err == nil && sent >= 1 {
		return ops.OutcomeDelivered, "", nil
	}
	e.log.Warn("could not press Enter after the paste", "what", what, "sent", sent, "err", err)
	switch {
	case errors.Is(err, term.ErrPaneChanged):
		// The text is in the pane and was not submitted.
		return ops.OutcomePartial, proto.CodePaneChanged, nil
	case errors.Is(err, term.ErrNotFound):
		return ops.OutcomePartial, proto.CodeTargetNotFound, nil
	}
	return ops.OutcomeUnknown, proto.CodeInternal, nil
}

func (e *Executor) sendKeys(ctx context.Context, rq Request) (any, error) {
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.SendKeysArgs) (ops.WriteArgs, ops.Target) { return a.WriteArgs, a.Target })
	if err != nil {
		return nil, err
	}
	return e.paneWrite(ctx, rq, op, args.WriteArgs, args.Target, true, inputGuards,
		func(ctx context.Context, p *pane) (ops.Outcome, proto.Code, error) {
			return e.sendKeyList(ctx, p, args.Keys)
		})
}

// sendKeyList sends the keys. An argument error of the adapter before any key
// went out is returned as an error (nothing was sent); every other failure is
// an outcome.
func (e *Executor) sendKeyList(ctx context.Context, p *pane, keys []string) (ops.Outcome, proto.Code, error) {
	sent, err := e.host.SendKeys(ctx, p.ref, keys, p.ref)
	switch {
	case err != nil:
		e.log.Warn("send_keys failed", "sent", sent, "of", len(keys), "err", err)
		if sent == 0 && errors.Is(err, ops.ErrInvalidArgument) {
			return "", "", hostErr("send the keys", err)
		}
		o, r := failureOf(err, sent)
		return o, r, nil
	case sent < len(keys):
		return ops.FailureOutcome(sent > 0), proto.CodeInternal, nil
	}
	return ops.OutcomeDelivered, "", nil
}

// answer picks one option of the dialog the pane shows, and only of the dialog
// the caller reviewed (args.Expect). The binding is a KEEP guard: it is checked
// inside the send step, which paneWrite runs under the pane lock on a pane that
// was resolved and classified after the lock was taken, immediately before the
// keys go out. A binding that does not describe that pane (another pane,
// process or session generation, or another dialog) delivers nothing and is
// not forceable; force never reaches it, and the Worker never swaps in the
// current dialog for the stale one.
func (e *Executor) answer(ctx context.Context, rq Request) (any, error) {
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.AnswerArgs) (ops.WriteArgs, ops.Target) { return a.WriteArgs, a.Target })
	if err != nil {
		return nil, err
	}
	return e.paneWrite(ctx, rq, op, args.WriteArgs, args.Target, false,
		func(p *pane) []proto.Code {
			// No FORCE guard: a pane without the reviewed dialog is a
			// binding mismatch, which send reports as a KEEP refusal that
			// force cannot override.
			return nil
		},
		func(ctx context.Context, p *pane) (ops.Outcome, proto.Code, error) {
			if code := dialogMismatch(p, args.Expect); code != "" {
				e.log.Info("answer refused: the pane does not match what the caller reviewed", "reason", code.String())
				return ops.OutcomeFailedNotSent, code, nil
			}
			keys, kerr := answerKeys(p.class, args.Choice)
			if kerr != nil {
				return "", "", kerr
			}
			return e.sendKeyList(ctx, p, keys)
		})
}

// dialogMismatch compares the pane as it is now with what the caller
// reviewed. It returns pane_changed when the pane, its process or the session
// generation differ (legacy target_changed), dialog_changed when the pane is
// the same but shows another dialog or none (legacy view_changed), and "" when
// the binding holds. A zero session generation (the listing failed) never
// matches.
func dialogMismatch(p *pane, b ops.ReviewedDialog) proto.Code {
	if p.incomplete {
		// The screen cannot be read completely now, so nothing can show that the
		// dialog is the one that was reviewed.
		return proto.CodeViewIncomplete
	}
	if p.ref.ID != b.PaneID || p.ref.PID != b.PID || p.ref.Command != b.Command ||
		p.info.SessionCreated == 0 || p.info.SessionCreated != b.SessionCreated {
		return proto.CodePaneChanged
	}
	if p.info.State != ops.PaneStateDialog || p.info.DialogDigest == "" || p.info.DialogDigest != b.DialogDigest {
		return proto.CodeDialogChanged
	}
	return ""
}

// answerKeys returns the keys that select the named option.
//
// Precedence, which is part of the contract (spine AM-13): a choice made only
// of decimal digits is the 1-based POSITION of the option in the dialog the
// caller reviewed, before any label or key is looked at. The browser and the
// CLI send positions, and a dialog may well have numeric labels ("2", "4",
// "8"); the position 2 must never be read as the label "2" of the first option.
// A position outside the options is invalid_argument, not a guess. Only a
// choice that is not a decimal number is matched by option label ("Yes",
// case-insensitive) and then by the key the dialog shows ("y"). Without parsed
// options a single character is sent as that key; anything else cannot be
// mapped.
func answerKeys(r agentdetect.Result, choice string) ([]string, error) {
	c := strings.TrimSpace(choice)
	if len(r.Choices) > 0 && isDecimal(c) {
		n, err := strconv.Atoi(c)
		if err != nil || n < 1 || n > len(r.Choices) {
			return nil, apiErr(proto.CodeInvalidArgument, "", "the choice is a position outside the options of the dialog")
		}
		return r.Choices[n-1].Keys, nil
	}
	for _, o := range r.Choices {
		if strings.EqualFold(strings.TrimSpace(o.Label), c) {
			return o.Keys, nil
		}
	}
	for _, o := range r.Choices {
		if len(o.Keys) > 0 && strings.EqualFold(o.Keys[0], c) {
			return o.Keys, nil
		}
	}
	if len(r.Choices) == 0 && utf8.RuneCountInString(c) == 1 {
		return []string{c}, nil
	}
	return nil, apiErr(proto.CodeInvalidArgument, "", "the choice matches no option of the dialog")
}

// isDecimal reports whether s is one or more ASCII digits.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// setTask sets the @minion_task option of the session.
func (e *Executor) setTask(ctx context.Context, rq Request) (any, error) {
	op, args, err := prepareWrite(e, ctx, rq, func(a ops.SetTaskArgs) (ops.WriteArgs, ops.Target) { return a.WriteArgs, a.Target })
	if err != nil {
		return nil, err
	}
	return e.write(ctx, op, args.WriteArgs, args.Target, true, func(ctx context.Context) (ops.WriteResult, error) {
		// The session is the target here, whatever pane the caller named.
		ref, err := e.resolve(ctx, ops.Target{WorkerID: args.Target.WorkerID, SessionName: args.Target.SessionName})
		if err != nil {
			return ops.WriteResult{}, err
		}
		if err := e.checkPolicy(ctx, args.Target, true); err != nil {
			return ops.WriteResult{}, err
		}
		info := basicInfo(args.Target.SessionName, ref)
		if err := e.host.SetOption(ctx, args.Target.SessionName, taskOption, args.Task); err != nil {
			e.log.Warn("set_task failed", "err", err)
			if errors.Is(err, term.ErrNotFound) || errors.Is(err, ops.ErrInvalidArgument) {
				// The adapter established that nothing changed.
				return ops.WriteResult{}, hostErr("set the task", err)
			}
			// Any other failure (an ended context, a timed-out or broken
			// client call) may come after the option was set. The result is
			// unknown and the request id keeps it, so a repeat cannot
			// overwrite a newer task (AD-15).
			return ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: proto.CodeInternal.String(), Pane: &info}, nil
		}
		e.locate(ctx, &info)
		return ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &info}, nil
	})
}

// AuditOriginRefusal leaves the local audit row of a message-class request
// that the op server refused for its origin (F-01): the op, the target and the
// request id when the arguments show them, the reason pipe_origin_refused and
// the origin. reason is missing_origin, malformed_origin or not_accepted. It
// never records content. It implements [OriginRefusalAuditor].
func (e *Executor) AuditOriginRefusal(ctx context.Context, op string, args json.RawMessage, origin *proto.Origin, reason string) {
	var a struct {
		ops.WriteArgs
		Target ops.Target `json:"target"`
	}
	_ = json.Unmarshal(args, &a) // best effort: the row is useful without them
	rec := store.AuditRecord{
		Op: op, Outcome: "error", Reason: string(proto.CodePipeOriginRefused),
		RequestID: a.RequestID,
	}
	if !ops.ValidUUIDv7(rec.RequestID) {
		rec.RequestID = ""
	}
	if a.Target.SessionName != "" && a.Target.Validate() == nil {
		rec.Target = a.Target.String()
	}
	switch reason {
	case "missing_origin":
		rec.Origin = "missing"
	case "malformed_origin":
		rec.Origin = "malformed"
	default:
		rec.Origin = "malformed"
		if origin != nil {
			if b, err := json.Marshal(origin); err == nil {
				rec.Origin = string(b)
			}
		}
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if aerr := e.state.AppendAudit(bctx, rec); aerr != nil {
		e.log.Warn("cannot write the local audit", "op", op, "err", aerr)
	}
}
