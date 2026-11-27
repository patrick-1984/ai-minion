package authz

import (
	"context"
	"slices"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// File transfer authority (files.v1, BRD minion-file-transfer FR-10, FR-11,
// 5.6).
//
// The two actions ops.ActionFileRead (list and pull) and ops.ActionFileWrite
// (push) are held:
//
//   - by an administrator on every resource and by the owner on their own
//     resources (allActions, ownerActions), subject to the user-managed switch;
//   - by an API key only when its stored actions name them (scope and base intersected): a
//     resource scope of "everything" implies no action, and a key stored
//     before v0.24 names none;
//   - never by a Worker identity through the own-Worker rule, a
//     session-control grant (GrantActions) or a direct grant's level;
//   - by a Worker session principal only through file flags, and only when
//     its calling session is verified: [Principal.WithVerifiedFiles], which
//     the Hub calls on the principal of a bound master lease (verifiedSource
//     in internal/hub/workers/master_calls.go). A claimed source (the
//     source_session a call stream names) never carries file authority,
//     whatever flags its grants have.
//
// The flags that count are, per target session: files_pull (file_read) and
// files_push (file_write, Control only) of a direct grant that is valid for
// that session now, and files_pull / files_push of the master acceptance row
// of the target Worker for the calling Worker, when that row covers the
// session (sessions_json NULL covers all) and the master scope reaches the
// session with read (pull) or operate (push). A flag applies only through its
// own row: an acceptance through accept_all has no row and no flag.

// FileAcceptance is one master acceptance row (control_accept) as a verified
// master call stands on it: TargetWorkerID accepts the calling Worker.
type FileAcceptance struct {
	// ID is the identity of the row (store.AcceptanceID), named in the
	// decision and the transfer record.
	ID string
	// TargetWorkerID is the accepting Worker.
	TargetWorkerID string
	// Sessions limits the row to some sessions of the target; nil means every
	// session (the wildcard row, sessions_json IS NULL), an empty list none.
	Sessions []string
	// FilesPull and FilesPush are the file transfer flags of the row.
	FilesPull, FilesPush bool
	// AppsInstall is the app install flag of the row (v0.25.0).
	AppsInstall bool
}

// covers reports whether the row reaches the session res.
func (a FileAcceptance) covers(res ResolvedResource) bool {
	if res.Kind != ResourceSession || a.TargetWorkerID == "" || a.TargetWorkerID != res.WorkerID {
		return false
	}
	return a.Sessions == nil || slices.Contains(a.Sessions, res.SessionName)
}

// fileAuthority is the verified calling session of a Worker principal and the
// master acceptances its call stands on. It is held by pointer so that
// Principal stays comparable.
type fileAuthority struct {
	session string
	accepts []FileAcceptance
}

// WithVerifiedFiles returns p with file authority for its calling session,
// which the Hub verified (the session of a bound master lease): the file
// flags of p's direct grants count from now on, and so do those of accepts,
// the master acceptance rows of the target Workers for p's Worker. Only a
// Worker principal can carry it; for any other principal, an empty session,
// or a session other than the one p already names ([Principal.SessionName]),
// p is returned unchanged and holds no file action (fail closed).
//
// Never call it for a source that the calling Worker only named (a call
// stream's source_session): that source is claimed, not verified.
func (p Principal) WithVerifiedFiles(session string, accepts []FileAcceptance) Principal {
	if p.Kind != KindWorker || session == "" || p.direct != nil && p.direct.session != session {
		return p
	}
	p.files = &fileAuthority{session: session, accepts: slices.Clone(accepts)}
	return p
}

// VerifiedFiles reports whether p carries verified file authority, and for
// which calling session.
func (p Principal) VerifiedFiles() (session string, ok bool) {
	if p.Kind != KindWorker || p.files == nil {
		return "", false
	}
	return p.files.session, true
}

// fileFlagsActions returns file_read for pull and file_write for push, when
// held (the actions the row authorizes otherwise) allows them: pull needs
// read, push needs operate.
func fileFlagsActions(pull, push bool, held []ops.Action) []ops.Action {
	var out []ops.Action
	if pull && slices.Contains(held, ops.ActionRead) {
		out = append(out, ops.ActionFileRead)
	}
	if push && slices.Contains(held, ops.ActionOperate) {
		out = append(out, ops.ActionFileWrite)
	}
	return out
}

// directFileFor is what the file flags of direct grant g authorize on res: only
// for a principal with verified file authority, only on a session, and only
// while g is valid for res (directFor). files_push needs level Control.
func (s subject) directFileFor(g DirectGrant, res ResolvedResource) []ops.Action {
	if s.p.Kind != KindWorker || s.p.files == nil || res.Kind != ResourceSession || !g.FilesPull && !g.FilesPush {
		return nil
	}
	push := g.FilesPush && g.Level == LevelControl
	return fileFlagsActions(g.FilesPull, push, s.directFor(g, res))
}

// acceptFileFor is what the file flags of acceptance row a authorize on res:
// only for a principal with verified file authority, only on a session of
// another Worker that the row covers, and only as far as the master scope
// (the owner's base permission intersected with Scope, grantActions) reaches the session.
func (s subject) acceptFileFor(a FileAcceptance, res ResolvedResource) []ops.Action {
	if s.p.Kind != KindWorker || s.p.files == nil || !a.FilesPull && !a.FilesPush {
		return nil
	}
	if !a.covers(res) || res.WorkerID == s.p.WorkerID {
		return nil
	}
	return fileFlagsActions(a.FilesPull, a.FilesPush, s.grantActions(res))
}

// appInstallActions is what the apps_install flags of the principal's
// verified direct grants and acceptance rows authorize on res: app_install on
// a Worker resource that is not the principal's own Worker.
func (s subject) appInstallActions(res ResolvedResource) []ops.Action {
	for _, g := range s.p.directList() {
		if s.directAppFor(g, res) {
			return []ops.Action{ops.ActionAppInstall}
		}
	}
	if s.p.files != nil {
		for _, a := range s.p.files.accepts {
			if s.acceptAppFor(a, res) {
				return []ops.Action{ops.ActionAppInstall}
			}
		}
	}
	return nil
}

// appInstallTarget reports whether res is a Worker on which the apps_install
// flags of the principal can count: only for a principal with verified file
// authority, and never on its own Worker.
func (s subject) appInstallTarget(res ResolvedResource) bool {
	return s.p.Kind == KindWorker && s.p.files != nil && res.Kind == ResourceWorker && res.WorkerID != s.p.WorkerID
}

// directAppFor reports whether the apps_install flag of direct grant g
// authorizes app_install on res: at level Control, while g is valid for the
// Worker (directFor).
func (s subject) directAppFor(g DirectGrant, res ResolvedResource) bool {
	return s.appInstallTarget(res) && g.AppsInstall && g.Level == LevelControl && len(s.directFor(g, res)) != 0
}

// acceptAppFor reports whether the apps_install flag of acceptance row a
// authorizes app_install on res: a row of that Worker, while the master scope
// reaches it.
func (s subject) acceptAppFor(a FileAcceptance, res ResolvedResource) bool {
	return s.appInstallTarget(res) && a.AppsInstall && a.TargetWorkerID == res.WorkerID && len(s.grantActions(res)) != 0
}

// AppInstallBasis tells what authorizes app_install by p on the Worker res
// now, for the audit of the caller: the direct grants whose apps_install flag
// authorizes it by themselves and the first acceptance row whose flag does,
// as [FileBasis] names them for a transfer (Standing for an administrator,
// the owner or an API key that names the action). It is empty for a
// principal without access and for a Worker that does not exist; only a
// failure of a lookup is an error. The Hub calls it after
// [Authorizer.Decide] allowed the operation; it reads the current state
// again, like Decide.
func (a *Authorizer) AppInstallBasis(ctx context.Context, p Principal, res Resource) (FileBasis, error) {
	sub, resolved, ok, err := a.grantSubject(ctx, p, res)
	if err != nil || !ok {
		return FileBasis{}, err
	}
	var b FileBasis
	if p.Kind != KindWorker {
		b.Standing = slices.Contains(sub.actions(resolved), ops.ActionAppInstall)
		return b, nil
	}
	for _, g := range p.directList() {
		if sub.directAppFor(g, resolved) {
			b.GrantIDs = append(b.GrantIDs, g.ID)
		}
	}
	if p.files != nil {
		for _, acc := range p.files.accepts {
			if sub.acceptAppFor(acc, resolved) {
				b.AcceptanceID = acc.ID
				break
			}
		}
	}
	return b, nil
}

// fileActions is the union of what the file flags of the principal's direct
// grants and acceptance rows authorize on res.
func (s subject) fileActions(res ResolvedResource) []ops.Action {
	if s.p.Kind != KindWorker || s.p.files == nil || res.Kind != ResourceSession {
		return nil
	}
	var out []ops.Action
	for _, g := range s.p.directList() {
		out = append(out, s.directFileFor(g, res)...)
	}
	for _, a := range s.p.files.accepts {
		out = append(out, s.acceptFileFor(a, res)...)
	}
	return out
}

// FileBasis tells what authorized a file action of a decided call, for the
// transfer record, the audit and the re-decisions during the transfer.
type FileBasis struct {
	// Standing is true when the principal holds the action by its own
	// standing: an administrator, the owner, an act-as session or an API key
	// that names the action.
	Standing bool
	// GrantIDs are the direct grants whose file flags authorize it by
	// themselves, in the order the principal carries them.
	GrantIDs []string
	// AcceptanceID is the first acceptance row whose file flags authorize it
	// ("" for none), also when the row is a wildcard row.
	AcceptanceID string
}

// Any reports whether something authorizes the action.
func (b FileBasis) Any() bool { return b.Standing || len(b.GrantIDs) > 0 || b.AcceptanceID != "" }

// FileActionFor returns the action a transfer of direction needs on the
// target session: file_read for a pull, file_write for a push. ok is false
// for an unknown direction.
func FileActionFor(direction string) (ops.Action, bool) {
	switch direction {
	case ops.FileDirectionPull:
		return ops.ActionFileRead, true
	case ops.FileDirectionPush:
		return ops.ActionFileWrite, true
	}
	return "", false
}

// DecideFile decides a file transfer of direction by p on the target session
// res: p must hold the action of [FileActionFor] there now. It returns, when
// it allows, the owner of the target as read for this decision and what
// authorized it. A refusal is the *proto.Error of [Authorizer.Decide]
// (not_found when p holds nothing on res, forbidden otherwise; an unknown
// direction is invalid_argument). Policies (Hub-wide, per Worker, local
// switches) are not part of it: see [CheckFileEnds].
func (a *Authorizer) DecideFile(ctx context.Context, p Principal, direction string, res SessionResource) (owner string, basis FileBasis, err error) {
	act, ok := FileActionFor(direction)
	if !ok {
		return "", FileBasis{}, proto.NewError(proto.CodeInvalidArgument, "unknown transfer direction")
	}
	pt := a.ports()
	sub, ok, err := a.subject(ctx, p, pt)
	if err != nil {
		return "", FileBasis{}, err
	}
	if !ok {
		return "", FileBasis{}, proto.NewError(proto.CodeForbidden, "not allowed")
	}
	resolved, err := res.resolve(ctx, pt)
	if err != nil {
		return "", FileBasis{}, mapLookupErr(err)
	}
	h, err := held(ctx, sub, resolved, pt)
	if err != nil {
		return "", FileBasis{}, err
	}
	if !slices.Contains(h, act) {
		if len(h) == 0 {
			return "", FileBasis{}, proto.NewError(proto.CodeNotFound, "not found")
		}
		return "", FileBasis{}, proto.NewError(proto.CodeForbidden, "not allowed")
	}
	basis = sub.fileBasis(act, resolved)
	if !basis.Any() {
		// held allowed it, so something must stand behind it; fail closed
		// if the two computations ever disagree.
		return "", FileBasis{}, proto.NewError(proto.CodeForbidden, "not allowed")
	}
	return resolved.OwnerID, basis, nil
}

// fileBasis computes what authorizes act on res for the subject. It does not
// apply the user-managed switch: the caller did with held.
func (s subject) fileBasis(act ops.Action, res ResolvedResource) FileBasis {
	var b FileBasis
	if s.p.Kind != KindWorker {
		b.Standing = slices.Contains(s.actions(res), act)
		return b
	}
	for _, g := range s.p.directList() {
		if slices.Contains(s.directFileFor(g, res), act) {
			b.GrantIDs = append(b.GrantIDs, g.ID)
		}
	}
	if s.p.files != nil {
		for _, a := range s.p.files.accepts {
			if slices.Contains(s.acceptFileFor(a, res), act) {
				b.AcceptanceID = a.ID
				break
			}
		}
	}
	return b
}

// FileEnd is the Hub's view of one end of a transfer.
type FileEnd struct {
	// WorkerID is the Worker of the end; empty for a REST or browser caller,
	// which has no Hub-side end (its local disk).
	WorkerID string
	// Mode is the Hub's per-Worker override (ops.FileMode*); empty reads as
	// ops.FileModeOn.
	Mode string
	// PreviewOnly is the Hub-side preview_only policy of the Worker: it
	// refuses receive and allows send.
	PreviewOnly bool
}

// allows reports the refusal of effect on the end, nil when allowed. An end
// without a Worker allows everything.
func (e FileEnd) allows(effect string) error {
	if e.WorkerID == "" {
		return nil
	}
	mode := e.Mode
	if mode == "" {
		mode = ops.FileModeOn
	}
	if !ops.WorkerFileModeAllows(mode, effect) {
		return proto.NewError(proto.CodeFilesDisabled, "file transfer is switched off for this direction on this Worker")
	}
	if effect == ops.FileEffectReceive && e.PreviewOnly {
		return proto.NewError(proto.CodePreviewOnly, "the Worker is preview only: files cannot be written to it")
	}
	return nil
}

// CheckFileEnds applies the Hub policies of a transfer of direction (FR-10,
// FR-18): the Hub-wide policy hubPolicy (empty reads as ops.FileModeOn;
// admins_only needs admin, whether the initiating principal is an
// administrator), and the per-Worker override and preview_only of each end
// for its effect: pull = target send + caller receive, push = caller send +
// target receive. A REST or browser caller (caller.WorkerID empty) has no
// Hub-side end. It returns nil when every layer allows, else a *proto.Error
// (files_disabled, preview_only, or invalid_argument for an unknown
// direction). The local switches of the Workers are their own check.
func CheckFileEnds(hubPolicy, direction string, admin bool, target, caller FileEnd) error {
	targetEffect, callerEffect, ok := ops.FileEffects(direction)
	if !ok {
		return proto.NewError(proto.CodeInvalidArgument, "unknown transfer direction")
	}
	if hubPolicy == "" {
		hubPolicy = ops.FileModeOn
	}
	if !ops.HubFilePolicyAllows(hubPolicy, direction, admin) {
		return proto.NewError(proto.CodeFilesDisabled, "file transfer in this direction is switched off on this Hub")
	}
	if target.WorkerID == "" {
		return proto.NewError(proto.CodeInvalidArgument, "a transfer needs a target Worker")
	}
	if err := target.allows(targetEffect); err != nil {
		return err
	}
	return caller.allows(callerEffect)
}
