package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
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
	// The Hub's logger (the one that feeds the log sinks) is used when the
	// core module runs; a kernel without it (tests) logs nothing.
	var opts []audit.Option
	if s, ok := env.k.services[typeOf[*Config]()]; ok {
		opts = append(opts, audit.WithLogger(s.value.(*Config).logger()))
	}
	opts = append(opts, audit.WithNamer(storeNamer(st)))
	log := audit.New(st, opts...)
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

// RecordSettingsEventTx implements SettingsTxAuditor: the row commits or
// rolls back with the settings change it describes.
func (a settingsAuditAdapter) RecordSettingsEventTx(ctx context.Context, ex store.Execer, e SettingsAuditEvent) error {
	return a.log.RecordTx(ctx, ex, audit.Event{Actor: e.Actor, ActingAs: e.ActingAs, Action: e.Action, Target: e.Target, Outcome: e.Outcome})
}

// storeNamer names the actors and targets of audit events for the audit
// page: a user id gives the user name, "worker:<id>" or a Worker id the Worker
// name. Anything else (system, key ids, free text) has no name.
func storeNamer(st *store.Store) func(context.Context, string) string {
	return func(ctx context.Context, id string) string {
		id = strings.TrimPrefix(id, "worker:")
		// "<worker id>/local", "<worker id>/session:<name>" and
		// "<worker id>:<session>" name the Worker and keep the rest.
		rest := ""
		if i := strings.IndexAny(id, "/:"); i >= 0 {
			id, rest = id[:i], id[i:]
		}
		if !ops.ValidUUIDv7(id) {
			return ""
		}
		if u, err := st.UserByID(ctx, id); err == nil && rest == "" {
			return u.Username
		}
		if w, err := st.WorkerByID(ctx, id); err == nil {
			return w.Name + rest
		}
		return ""
	}
}
