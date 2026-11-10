package server

import (
	"context"
	"fmt"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// auditModuleOrder places the audit module right after the store module
// (Story 3.2, which provides *store.Store) and before every module that
// records audit events.
const auditModuleOrder = 20

func init() { registerAuditModule(DefaultRegistry) }

// registerAuditModule registers the audit module in r. It requires the
// *store.Store service and provides the *audit.Log service.
func registerAuditModule(r *Registry) {
	if err := r.Register("audit", auditModuleOrder, initAudit, DeclaresService[*audit.Log](), DeclaresService[SettingsAuditor]()); err != nil {
		panic(err)
	}
}

func initAudit(_ context.Context, env *Env) error {
	st, err := Require[*store.Store](env)
	if err != nil {
		return err
	}
	log := audit.New(st)
	if err := Provide(env, log); err != nil {
		return fmt.Errorf("provide audit log: %w", err)
	}
	if err := Provide[SettingsAuditor](env, settingsAuditAdapter{log: log}); err != nil {
		return fmt.Errorf("provide settings auditor: %w", err)
	}
	return nil
}

// settingsAuditAdapter records settings changes in the audit log.
type settingsAuditAdapter struct{ log *audit.Log }

// RecordSettingsEvent implements SettingsAuditor.
func (a settingsAuditAdapter) RecordSettingsEvent(ctx context.Context, e SettingsAuditEvent) error {
	return a.log.Record(ctx, audit.Event{Actor: e.Actor, ActingAs: e.ActingAs, Action: e.Action, Target: e.Target, Outcome: e.Outcome})
}
