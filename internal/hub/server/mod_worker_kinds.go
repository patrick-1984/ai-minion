package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

// workerKindsModuleOrder places the module after the workers module (30) and
// the REST framework (40), whose service and handler registry it uses.
const workerKindsModuleOrder = 45

func init() { registerWorkerKindsModule(DefaultRegistry) }

// registerWorkerKindsModule registers the module of Story 6.6 in r. It
// requires the config, the store, the audit log, the authorizer, the workers
// service and the REST handler registry and
//
//   - builds the reassignment and the restriction services over the one
//     connection registry of the workers service;
//   - makes the workers service deliver the stored Hub-side policy
//     (preview_only) in the welcome of every Worker that connects;
//   - registers worker_reassign, worker_restrict and worker_rename, and
//     trust_set and trust_list on the REST handler registry.
//
// The policy hook is set during init: the Hub answers 503 to everything but
// the probes until the kernel has started, so no Worker connects before it.
func registerWorkerKindsModule(r *Registry) {
	err := r.Register("worker_kinds", workerKindsModuleOrder, initWorkerKinds,
		DeclaresService[*workers.Reassigner](),
		DeclaresService[*workers.Restrictions](),
		DeclaresService[*workers.Approver](),
		DeclaresService[*authz.Trust](),
	)
	if err != nil {
		panic(err)
	}
}

func initWorkerKinds(_ context.Context, env *Env) error {
	cfg, err := Require[*Config](env)
	if err != nil {
		return err
	}
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	auditLog, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	az, err := Require[*authz.Authorizer](env)
	if err != nil {
		return err
	}
	svc, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	handlers, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	reassigner, err := workers.NewReassigner(workers.ReassignConfig{
		Store: st, Connections: svc.Registry(), Auditor: auditLog, Logger: cfg.logger(),
	})
	if err != nil {
		return fmt.Errorf("build the reassignment service: %w", err)
	}
	restrictions, err := workers.NewRestrictions(workers.RestrictionsConfig{
		Store: st, Connections: svc.Registry(), Auditor: auditLog, Logger: cfg.logger(),
	})
	if err != nil {
		return fmt.Errorf("build the restriction service: %w", err)
	}
	approver, err := workers.NewApprover(workers.ApproverConfig{Store: st, Connections: svc.Registry(), Logger: cfg.logger()})
	if err != nil {
		return fmt.Errorf("build the approval service: %w", err)
	}
	if err := api.RegisterWorkerApprove(handlers, approver); err != nil {
		return fmt.Errorf("register worker_approve: %w", err)
	}
	trust := authz.NewTrust(api.TrustStore(st), nil)
	svc.UsePolicy(restrictions.Policy)
	if err := api.RegisterWorkerKindHandlers(handlers, reassigner, restrictions); err != nil {
		return fmt.Errorf("register the Worker administration operations: %w", err)
	}
	if err := api.RegisterTrustHandlers(handlers, trust, az); err != nil {
		return fmt.Errorf("register the trust operations: %w", err)
	}
	if err := Provide(env, reassigner); err != nil {
		return err
	}
	if err := Provide(env, restrictions); err != nil {
		return err
	}
	if err := Provide(env, approver); err != nil {
		return err
	}
	return Provide(env, trust)
}
