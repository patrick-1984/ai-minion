package server

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

func init() { registerBoundMasterModule(DefaultRegistry) }

func registerBoundMasterModule(r *Registry) {
	if err := r.Register("bound_master", eventsModuleOrder+1, initBoundMaster, DeclaresService[*workers.MasterControl]()); err != nil {
		panic(err)
	}
}

func initBoundMaster(_ context.Context, env *Env) error {
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	h, err := Require[*api.Registry](env)
	if err != nil {
		return err
	}
	service, err := Require[*workers.Service](env)
	if err != nil {
		return err
	}
	events, err := Require[*workers.EventHub](env)
	if err != nil {
		return err
	}
	ingest, err := Require[*workers.Ingest](env)
	if err != nil {
		return err
	}
	router, err := Require[*workers.Router](env)
	if err != nil {
		return err
	}
	log, err := Require[*audit.Log](env)
	if err != nil {
		return err
	}
	master := workers.NewMasterControl(st, service.Registry(), events, router, log)
	if err := master.Bind(ingest, service); err != nil {
		return err
	}
	if err := workers.NewLocalAudit(log).Bind(service); err != nil {
		return err
	}
	if err := api.RegisterBoundMaster(h, st, master.SetBoundMaster); err != nil {
		return err
	}
	return Provide(env, master)
}
