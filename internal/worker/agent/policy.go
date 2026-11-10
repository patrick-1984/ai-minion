package agent

import (
	"context"
	"sync/atomic"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// ShareSource is where the share selection comes from. *store.Store
// implements it.
type ShareSource interface {
	LoadShareContext(ctx context.Context) (store.ShareSelection, error)
}

// ShareStateSource is a [ShareSource] that also knows the revision of the
// share selection, read together with it. *store.Store implements it.
type ShareStateSource interface {
	LoadShareStateContext(ctx context.Context) (store.ShareState, error)
}

// ShareCommitter is a [ShareSource] that can change the selection and answers
// with what it committed. *store.Store implements it.
type ShareCommitter interface {
	CommitShareContext(ctx context.Context, sel store.ShareSelection) (store.ShareState, error)
}

// Policy is the Worker's local policy: preview-only and the share selection.
// It is enforced here whatever actions the Hub or a local caller sends
// (AD-12). It is safe for concurrent use; the settings layer may flip
// preview-only at runtime.
type Policy struct {
	share          ShareSource
	previewOnly    atomic.Bool
	hubPreviewOnly atomic.Bool
	shareLock      atomic.Bool
	workerID       atomic.Pointer[string]
}

// NewPolicy returns a Policy that reads the share selection from share. A nil
// share shares nothing.
func NewPolicy(share ShareSource) *Policy { return &Policy{share: share} }

// SetPreviewOnly turns preview-only on or off. With it on, every write is
// refused with forbidden/preview_only; reads still work.
func (p *Policy) SetPreviewOnly(on bool) { p.previewOnly.Store(on) }

// SetHubPreviewOnly sets the restriction the Hub delivered in welcome
// (Story 4.4, AD-12). It is a separate layer so that the Hub can lift its own
// restriction without ever lifting the local one.
func (p *Policy) SetHubPreviewOnly(on bool) { p.hubPreviewOnly.Store(on) }

// HubPreviewOnly reports whether the Hub restricts this Worker to preview.
func (p *Policy) HubPreviewOnly() bool { return p.hubPreviewOnly.Load() }

// PreviewOnly reports whether the Worker is preview-only: the local setting
// or the Hub's restriction. Local restrictions always win, because the Hub can
// only add a restriction, never remove the local one.
func (p *Policy) PreviewOnly() bool { return p.previewOnly.Load() || p.hubPreviewOnly.Load() }

// SetWorkerID sets the id the Hub gave this Worker ("" when it has none, or
// after the enrollment was removed). The connector sets it from the stored
// enrollment and from every welcome. It is what an operation that names a Worker
// (share_get, share_set) is held to: see [Executor] requireSelf.
func (p *Policy) SetWorkerID(id string) { p.workerID.Store(&id) }

// WorkerID returns the id the Hub gave this Worker, or "".
func (p *Policy) WorkerID() string {
	if id := p.workerID.Load(); id != nil {
		return *id
	}
	return ""
}

// SetShareLock turns the local share lock on or off (the setting share_lock).
// With it on, the Hub cannot change the share selection: share_set is refused
// with forbidden. The operator still changes it locally, and the lock is read
// from the settings at every start, so nothing the Hub sends can lift it.
func (p *Policy) SetShareLock(on bool) { p.shareLock.Store(on) }

// ShareLocked reports whether the local share lock is on.
func (p *Policy) ShareLocked() bool { return p.shareLock.Load() }

// Selection returns the current share selection. Nothing is shared without a
// source.
func (p *Policy) Selection(ctx context.Context) (store.ShareSelection, error) {
	if p.share == nil {
		return store.ShareSelection{Mode: store.ShareNone}, nil
	}
	return p.share.LoadShareContext(ctx)
}

// State returns the current share selection together with its revision. A
// source that has no revisions (a test fake) reports revision 0.
func (p *Policy) State(ctx context.Context) (store.ShareState, error) {
	if p.share == nil {
		return store.ShareState{ShareSelection: store.ShareSelection{Mode: store.ShareNone}}, nil
	}
	if src, ok := p.share.(ShareStateSource); ok {
		return src.LoadShareStateContext(ctx)
	}
	sel, err := p.share.LoadShareContext(ctx)
	return store.ShareState{ShareSelection: sel}, err
}

// Shared reports whether the named session is shared with callers (Hub and
// local). An unshared session is hidden from lists and refused for reads and
// writes alike (R75, A-9).
func (p *Policy) Shared(ctx context.Context, session string) (bool, error) {
	sel, err := p.Selection(ctx)
	if err != nil {
		return false, err
	}
	return sel.Includes(session), nil
}
