package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// AccountPrefsStore is the port of the preference handlers; *store.Store
// implements it.
type AccountPrefsStore interface {
	UserPrefs(ctx context.Context, userID string) (store.UserPrefs, error)
	SetAllowHiddenSessions(ctx context.Context, userID string, allow bool) error
	SetWantedHarnesses(ctx context.Context, userID string, names []string) error
}

var _ AccountPrefsStore = (*store.Store)(nil)

// RegisterAccountPrefs registers the handlers of account_prefs and
// account_prefs_set (v0.10.0) in r. hubPolicy returns the Hub setting
// hidden_sessions as it applies now.
//
// Both act on the caller's own account, the user of the principal: the
// session user, the owner of an API key, or the user an admin acts as. The
// dispatcher decides read (account_prefs) and operate (account_prefs_set) on
// that user resource, which the account's owner holds; a Worker identity has
// no account and is refused. The write is audited by the dispatcher.
//
// The opt-in to hidden sessions is account-wide: it lets every browser
// session and key of the account create them. So account_prefs_set takes
// more than operate (setPrefsAllowed): a signed-in browser session of the
// person themself, or an API key that holds manage on the account (dec
// decides it: an administrator's key whose scope grants manage). An
// administrator acting as someone else cannot persist that person's choice.
func RegisterAccountPrefs(r *Registry, st AccountPrefsStore, dec Decider, hubPolicy func() string, allows func(hub string, user bool) bool) error {
	if st == nil || dec == nil || hubPolicy == nil || allows == nil {
		return errors.New("api: the account preferences need a store, a decider and the hidden_sessions policy")
	}
	h := &accountPrefsHandlers{st: st, dec: dec, hub: hubPolicy, allows: allows}
	if err := HandleTyped(r, ops.OpAccountPrefs, h.get); err != nil {
		return fmt.Errorf("api: register account preferences: %w", err)
	}
	if err := HandleTyped(r, ops.OpAccountPrefsSet, h.set); err != nil {
		return fmt.Errorf("api: register account preferences: %w", err)
	}
	return nil
}

type accountPrefsHandlers struct {
	st     AccountPrefsStore
	dec    Decider
	hub    func() string
	allows func(hub string, user bool) bool
}

var errNoAccount = NewError(proto.CodeForbidden, "preferences belong to an account; this caller has none")

func (h *accountPrefsHandlers) get(ctx context.Context, c *Call, _ ops.AccountPrefsArgs) (ops.AccountPrefsResult, error) {
	if c.Principal.UserID == "" {
		return ops.AccountPrefsResult{}, errNoAccount
	}
	p, err := h.st.UserPrefs(ctx, c.Principal.UserID)
	if err != nil {
		return ops.AccountPrefsResult{}, err
	}
	hub := h.hub()
	return ops.AccountPrefsResult{
		AllowHiddenSessions: p.AllowHiddenSessions, HiddenSessions: hub,
		HiddenSessionsAllowed: h.allows(hub, p.AllowHiddenSessions), WantedHarnesses: p.WantedHarnesses,
	}, nil
}

// setPrefsAllowed refuses a caller that may not change the account-wide
// opt-in (see RegisterAccountPrefs).
func (h *accountPrefsHandlers) setPrefsAllowed(ctx context.Context, c *Call) error {
	p := c.Principal
	switch p.Kind {
	case authz.KindBrowserSession:
		if c.Identity.Session != nil {
			return nil
		}
	case authz.KindActAs:
		return NewError(proto.CodeForbidden, "an administrator acting as another user cannot change that user's preferences; the user changes them in their own Account page")
	case authz.KindAPIKey:
		err := h.dec.Decide(ctx, p, ops.MustActionSet(ops.ActionManage), authz.UserResource{ID: p.UserID})
		if err == nil {
			return nil
		}
		if e := ErrorOf(err); e.Code != proto.CodeForbidden && e.Code != proto.CodeNotFound {
			return err
		}
		return NewError(proto.CodeForbidden, "this API key cannot change your preferences: that takes a signed-in browser session (Account > Sessions) or a key that holds manage")
	}
	return errNoAccount
}

func (h *accountPrefsHandlers) set(ctx context.Context, c *Call, args ops.AccountPrefsSetArgs) (ops.WriteResult, error) {
	if c.Principal.UserID == "" {
		return ops.WriteResult{}, errNoAccount
	}
	if err := h.setPrefsAllowed(ctx, c); err != nil {
		return ops.WriteResult{}, err
	}
	if args.WantedHarnesses != nil {
		if err := h.st.SetWantedHarnesses(ctx, c.Principal.UserID, *args.WantedHarnesses); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ops.WriteResult{}, NewError(proto.CodeNotFound, "the account does not exist")
			}
			return ops.WriteResult{}, err
		}
	}
	if args.AllowHiddenSessions == nil {
		return delivered(), nil
	}
	if err := h.st.SetAllowHiddenSessions(ctx, c.Principal.UserID, *args.AllowHiddenSessions); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ops.WriteResult{}, NewError(proto.CodeNotFound, "the account does not exist")
		}
		return ops.WriteResult{}, err
	}
	res := delivered()
	if hub := h.hub(); h.allows(hub, true) == h.allows(hub, false) {
		res.Warnings = []ops.Warning{{Code: WarnHubPolicy,
			Message: "saved, but the Hub setting hidden_sessions is " + hub + ", so your choice applies only while it is user-controlled"}}
	}
	return res, nil
}

// WarnHubPolicy says that a saved preference has no effect now because a Hub
// setting decides for everybody.
const WarnHubPolicy = "hub_policy"
