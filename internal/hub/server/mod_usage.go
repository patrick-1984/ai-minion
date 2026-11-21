package server

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func init() { RegisterModule("usage", 59, initUsage) }

// Private organizational data belongs to the person. Act-as, worker identity,
// and keys limited to a subset of resources cannot browse the person's private
// bucket definitions or use a bucket ID as a membership oracle.
func usageBucketOwner(p authz.Principal) string {
	switch p.Kind {
	case authz.KindBrowserSession:
		return p.UserID
	case authz.KindAPIKey:
		if p.Scope != nil && slices.Contains(p.Scope.Actions(authz.ResolvedResource{Kind: authz.ResourceUser, UserID: p.UserID, OwnerID: p.UserID}), ops.ActionRead) {
			// A chosen-users scope covering the owner may cover unrelated
			// organization, including members on other users' Workers for an
			// admin. Keep private organization browser-only for limited keys.
			probe := store.NewID()
			if slices.Contains(p.Scope.Actions(authz.ResolvedResource{Kind: authz.ResourceUser, UserID: probe, OwnerID: probe}), ops.ActionRead) {
				return p.UserID
			}
		}
	}
	return ""
}

func usageForbidden() error {
	return api.NewError(proto.CodeForbidden, "private usage buckets require the owner's own session or full-scope key")
}

func initUsage(_ context.Context, env *Env) error {
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	r, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	rt, err := Require[*workers.Router](env)
	if err != nil {
		return err
	}

	if err = api.HandleTyped(r, ops.OpUsageList, func(ctx context.Context, c *api.Call, a ops.UsageListArgs) (ops.UsageListResult, error) {
		owner := usageBucketOwner(c.Principal)
		if a.BucketID != "" && owner == "" {
			return ops.UsageListResult{}, usageForbidden()
		}
		allowed, err := rt.ListWorkers(ctx, c.Principal, true)
		if err != nil {
			return ops.UsageListResult{}, err
		}
		return st.ReadUsage(ctx, allowed, owner, a, time.Now())
	}); err != nil {
		return err
	}

	if err = api.HandleTyped(r, ops.OpUsageBucketList, func(ctx context.Context, c *api.Call, _ ops.UsageBucketListArgs) (ops.UsageBucketListResult, error) {
		owner := usageBucketOwner(c.Principal)
		if owner == "" {
			return ops.UsageBucketListResult{}, usageForbidden()
		}
		buckets, err := st.UsageBuckets(ctx, owner)
		return ops.UsageBucketListResult{Buckets: buckets}, err
	}); err != nil {
		return err
	}

	return api.HandleTyped(r, ops.OpUsageBucketSet, func(ctx context.Context, c *api.Call, a ops.UsageBucketSetArgs) (ops.UsageBucketSetResult, error) {
		owner := usageBucketOwner(c.Principal)
		if owner == "" {
			return ops.UsageBucketSetResult{}, usageForbidden()
		}
		allowed, err := rt.ListWorkers(ctx, c.Principal, true)
		if err != nil {
			return ops.UsageBucketSetResult{}, err
		}
		res, err := st.SetUsageBucket(ctx, owner, allowed, a)
		if errors.Is(err, store.ErrNotFound) {
			err = api.NewError(proto.CodeNotFound, "unknown usage bucket")
		}
		return res, err
	})
}
