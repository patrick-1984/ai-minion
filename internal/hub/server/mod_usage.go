package server

import (
	"context"
	"errors"
	"fmt"
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
		scope, ok := p.Scope.(interface{ AllResources() bool })
		if ok && scope.AllResources() && slices.Contains(p.Scope.Actions(authz.ResolvedResource{Kind: authz.ResourceUser, UserID: p.UserID, OwnerID: p.UserID}), ops.ActionRead) {
			return p.UserID
		}
	}
	return ""
}

func usageForbidden() error {
	return api.NewError(proto.CodeForbidden, "private usage buckets require the owner's own session or full-scope key")
}

// usageDomainAuthority is the authority to change what the whole Hub
// publishes: the principal's own session or full-scope key (never act-as, a
// Worker identity or a restricted key, which usageBucketOwner refuses) whose
// owner holds the manage permission right now. The store checks again inside
// its transaction that the actor is still an active administrator.
func usageDomainAuthority(ctx context.Context, az *authz.Authorizer, p authz.Principal) store.BucketAuthority {
	if usageBucketOwner(p) == "" {
		return store.BucketAuthority{}
	}
	err := az.Decide(ctx, p, ops.MustActionSet(ops.ActionManage), authz.UserResource{ID: p.UserID})
	return store.BucketAuthority{Actor: p.UserID, Domain: err == nil}
}

func usageNominationError(err error) error {
	var refusal *store.NominationRefusal
	switch {
	case errors.As(err, &refusal):
		return api.NewError(proto.CodeConflict, fmt.Sprintf("this bucket cannot be published: %d of its members come from other users' sessions or accounts whose owners have not allowed republishing (they can allow it in their own usage settings). Remove those members or ask the owners.", refusal.Members))
	case errors.Is(err, store.ErrNominationForbidden):
		return api.NewError(proto.CodeForbidden, "publishing to or changing a bucket published to every Worker needs the manage permission of an active administrator, with your own session or a full-scope key")
	case errors.Is(err, store.ErrNotFound):
		return api.NewError(proto.CodeNotFound, "unknown usage bucket")
	}
	return err
}

func usageUserResource(p authz.Principal, _ any) (authz.Resource, error) {
	if p.UserID == "" {
		return nil, api.NewError(proto.CodeForbidden, "a signed-in usage account is required")
	}
	return authz.UserResource{ID: p.UserID}, nil
}

func usageListResource(p authz.Principal, args any) (authz.Resource, error) {
	if a, ok := args.(*ops.UsageListArgs); ok && a.WorkerID != "" && p.Kind == authz.KindAPIKey {
		return authz.WorkerResource{ID: a.WorkerID}, nil
	}
	return usageUserResource(p, args)
}

func initUsage(ctx context.Context, env *Env) error {
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
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}

	if err := registerUsageCollection(ctx, env, st, r, rt); err != nil {
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
	}, api.WithResource(usageListResource)); err != nil {
		return err
	}

	if err = api.HandleTyped(r, ops.OpUsageBucketList, func(ctx context.Context, c *api.Call, _ ops.UsageBucketListArgs) (ops.UsageBucketListResult, error) {
		owner := usageBucketOwner(c.Principal)
		if owner == "" {
			return ops.UsageBucketListResult{}, usageForbidden()
		}
		buckets, err := st.UsageBuckets(ctx, owner)
		if err != nil {
			return ops.UsageBucketListResult{}, err
		}
		allow, err := st.UsageAllowRepublish(ctx, owner)
		return ops.UsageBucketListResult{Buckets: buckets, AllowRepublish: allow}, err
	}); err != nil {
		return err
	}

	if err = api.HandleTyped(r, ops.OpUsageNominationSet, func(ctx context.Context, c *api.Call, a ops.UsageNominationSetArgs) (ops.UsageNominationSetResult, error) {
		owner := usageBucketOwner(c.Principal)
		if owner == "" {
			return ops.UsageNominationSetResult{}, usageForbidden()
		}
		var auth store.BucketAuthority
		if a.Scope == ops.UsageScopeDomain {
			auth = usageDomainAuthority(ctx, az, c.Principal)
		}
		res, err := st.SetUsageNomination(ctx, owner, auth, a)
		return res, usageNominationError(err)
	}, api.WithResource(usageUserResource)); err != nil {
		return err
	}

	if err = api.HandleTyped(r, ops.OpUsageNominationList, func(ctx context.Context, c *api.Call, _ ops.UsageNominationListArgs) (ops.UsageNominationListResult, error) {
		if !usageDomainAuthority(ctx, az, c.Principal).Domain {
			return ops.UsageNominationListResult{}, usageNominationError(store.ErrNominationForbidden)
		}
		slots, err := st.UsageDomainSlots(ctx)
		return ops.UsageNominationListResult{Domain: slots}, err
	}, api.WithResource(usageUserResource)); err != nil {
		return err
	}

	if err = api.HandleTyped(r, ops.OpUsagePublicationSet, func(ctx context.Context, c *api.Call, a ops.UsagePublicationSetArgs) (ops.UsagePublicationSetArgs, error) {
		owner := usageBucketOwner(c.Principal)
		if owner == "" {
			return ops.UsagePublicationSetArgs{}, usageForbidden()
		}
		return a, st.SetUsagePublication(ctx, owner, a.AllowRepublish)
	}, api.WithResource(usageUserResource)); err != nil {
		return err
	}

	if err := api.HandleTyped(r, ops.OpUsageCollectorSet, func(ctx context.Context, c *api.Call, a ops.UsageCollectorSetArgs) (ops.UsageCollector, error) {
		if usageBucketOwner(c.Principal) == "" {
			return ops.UsageCollector{}, usageForbidden()
		}
		allowed, err := rt.ListWorkers(ctx, c.Principal, true)
		if err != nil {
			return ops.UsageCollector{}, err
		}
		res, err := st.SetUsageCollector(ctx, allowed, a)
		if errors.Is(err, store.ErrNotFound) {
			err = api.NewError(proto.CodeNotFound, "account or collector source is unavailable")
		} else if errors.Is(err, store.ErrUsageNoNativeReader) {
			err = api.NewError(proto.CodeConflict, err.Error())
		}
		return res, err
	}, api.WithResource(usageUserResource)); err != nil {
		return err
	}

	if err := api.HandleTyped(r, ops.OpUsageAccountSet, func(ctx context.Context, c *api.Call, a ops.UsageAccountSetArgs) (ops.UsageAccountSetArgs, error) {
		owner := usageBucketOwner(c.Principal)
		if owner == "" {
			return ops.UsageAccountSetArgs{}, usageForbidden()
		}
		allowed, err := rt.ListWorkers(ctx, c.Principal, true)
		if err != nil {
			return ops.UsageAccountSetArgs{}, err
		}
		err = st.SetUsageAccount(ctx, owner, allowed, a)
		if errors.Is(err, store.ErrNotFound) {
			err = api.NewError(proto.CodeNotFound, "account not available")
		}
		return a, err
	}, api.WithResource(usageUserResource)); err != nil {
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
		res, err := st.SetUsageBucketAs(ctx, owner, allowed, a, usageDomainAuthority(ctx, az, c.Principal))
		return res, usageNominationError(err)
	})
}
