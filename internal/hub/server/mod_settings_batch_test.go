package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// txService is a settings service over the real store and the real audit log,
// so that a change and its audit rows share one transaction.
func (f *fixture) txService() *SettingsService {
	f.t.Helper()
	svc, err := NewSettingsService(context.Background(), SettingsDeps{
		Store: f.st, Auditor: settingsAuditAdapter{log: audit.New(f.st)}, Registry: testRegistry(),
		Live: func(k string) bool { return k != keyMode && k != keyVault }, LookupEnv: f.env.lookup,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return svc
}

func (f *fixture) auditRows() []store.AuditRow {
	f.t.Helper()
	rows, err := f.st.ListAudit(context.Background(), store.AuditQuery{Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func batchArgs(changes ...ops.SettingChange) ops.SettingsSetArgs {
	return ops.SettingsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Changes: changes}
}

func TestBatchIsStoredAndAuditedTogether(t *testing.T) {
	f := newFixture(t)
	svc := f.txService()
	var applied []string
	svc.OnChange([]string{keyGreeting, "api_"}, nil, func(_ context.Context, eff *config.Resolver) error {
		g, _ := eff.Resolve(keyGreeting)
		tok, _ := eff.Resolve(keyToken)
		applied = append(applied, g.Value+"/"+tok.Value)
		return nil
	})
	res, err := svc.Set(context.Background(), admin, batchArgs(
		ops.SettingChange{Key: keyGreeting, Value: "hi"},
		ops.SettingChange{Key: keyToken, Value: secretValue},
	))
	if err != nil || res.Outcome != ops.OutcomeDelivered || len(res.Warnings) != 0 {
		t.Fatalf("Set = %+v, %v", res, err)
	}
	// apply ran once, after the commit, and saw both values.
	if len(applied) != 1 || applied[0] != "hi/"+secretValue {
		t.Fatalf("applied = %q", applied)
	}
	rows := f.auditRows()
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Action != AuditActionSettingSet || r.Outcome != AuditOutcomeOK || r.Actor != admin.ID || strings.Contains(r.Target, secretValue) {
			t.Errorf("row = %+v", r)
		}
	}
	// A batch with a restart-only key warns once.
	res, err = svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyMode, Value: "slow"}, ops.SettingChange{Key: keyGreeting, Unset: true}))
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != ops.WarnRestartRequired {
		t.Fatalf("Set = %+v, %v", res, err)
	}
	if r := resolve(t, svc, keyGreeting); r.Value != "hello" || r.Source != config.SourceDefault {
		t.Fatalf("greeting = %+v", r)
	}
}

func TestFailingValidatorLeavesStoreAndAuditUntouched(t *testing.T) {
	f := newFixture(t)
	svc := f.txService()
	var seen []string
	svc.OnChange([]string{keyGreeting}, func(p *config.Resolver) error {
		g, _ := p.Resolve(keyGreeting)
		tok, _ := p.Resolve(keyToken)
		seen = append(seen, g.Value+"/"+tok.Value)
		if g.Value == "bad" {
			return errors.New("setting \"greeting\" is not acceptable with api_token")
		}
		return nil
	}, func(context.Context, *config.Resolver) error { t.Fatal("apply after a refused change"); return nil })
	_, err := svc.Set(context.Background(), admin, batchArgs(
		ops.SettingChange{Key: keyToken, Value: secretValue},
		ops.SettingChange{Key: keyGreeting, Value: "bad"},
	))
	var ae *ops.ArgumentError
	if !errors.As(err, &ae) || ae.Field != "changes" || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("err = %v", err)
	}
	// The validator saw the proposed view: both new values together.
	if len(seen) != 1 || seen[0] != "bad/"+secretValue {
		t.Fatalf("validator saw %q", seen)
	}
	if rows := f.auditRows(); len(rows) != 0 {
		t.Fatalf("audit rows after a refused change: %+v", rows)
	}
	if recs, _ := f.st.ListSettings(context.Background()); len(recs) != 0 {
		t.Fatalf("stored after a refused change: %+v", recs)
	}
}

func TestBatchIsAllOrNothing(t *testing.T) {
	f := newFixture(t)
	svc := f.txService()
	for _, bad := range []ops.SettingChange{{Key: "no_such_key", Value: "x"}, {Key: keyMode, Value: "medium"}, {Key: config.KeyDataDir, Value: "/x"}, {Key: keyWorker, Value: "x"}} {
		_, err := svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyGreeting, Value: "hi"}, bad))
		var ae *ops.ArgumentError
		if !errors.As(err, &ae) || !strings.HasPrefix(ae.Field, "changes[1].") {
			t.Fatalf("%+v: err = %v", bad, err)
		}
	}
	f.env["MINION_GREETING"] = "from-env"
	if _, err := svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyMode, Value: "slow"}, ops.SettingChange{Key: keyGreeting, Value: "x"})); err == nil {
		t.Fatal("a batch with an env-set key was accepted")
	}
	if recs, _ := f.st.ListSettings(context.Background()); len(recs) != 0 {
		t.Fatalf("stored: %+v", recs)
	}
	rows := f.auditRows()
	if len(rows) != 1 || rows[0].Outcome != AuditOutcomeRefused || rows[0].Target != keyGreeting {
		t.Fatalf("audit rows = %+v", rows)
	}
}

func TestBatchArgumentValidation(t *testing.T) {
	for name, a := range map[string]ops.SettingsSetArgs{
		"mixed":     {Key: keyGreeting, Changes: []ops.SettingChange{{Key: keyMode, Value: "x"}}},
		"duplicate": {Changes: []ops.SettingChange{{Key: keyMode, Value: "x"}, {Key: keyMode, Unset: true}}},
		"bad key":   {Changes: []ops.SettingChange{{Key: "Bad Key"}}},
		"both":      {Changes: []ops.SettingChange{{Key: keyMode, Value: "x", Unset: true}}},
	} {
		a.RequestID = ops.NewUUIDv7()
		if err := a.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Concurrent changes serialise: each validate-commit-apply completes before
// the next starts, so apply sees the values in commit order.
func TestConcurrentBatchesSerialise(t *testing.T) {
	f := newFixture(t)
	svc := f.txService()
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	enter := func() {
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
	}
	leave := func() { mu.Lock(); inFlight--; mu.Unlock() }
	var last string
	svc.OnChange([]string{keyGreeting}, func(*config.Resolver) error { enter(); return nil },
		func(_ context.Context, eff *config.Resolver) error {
			g, _ := eff.Resolve(keyGreeting)
			tok, _ := eff.Resolve(keyToken)
			// Each batch sets both keys to the same suffix: an interleaved
			// commit would show a mismatch.
			if strings.TrimPrefix(g.Value, "g") != strings.TrimPrefix(tok.Value, "t") {
				t.Errorf("mixed state %s / %s", g.Value, tok.Value)
			}
			last = g.Value
			leave()
			return nil
		})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := string(rune('a' + i))
			if _, err := svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyGreeting, Value: "g" + n}, ops.SettingChange{Key: keyToken, Value: "t" + n})); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxInFlight != 1 || last == "" {
		t.Fatalf("max in flight %d, last %q", maxInFlight, last)
	}
	if n := len(f.auditRows()); n != 40 {
		t.Fatalf("%d audit rows, want 40", n)
	}
}

func TestApplyFailureIsAWarning(t *testing.T) {
	f := newFixture(t)
	svc := f.txService()
	svc.OnChange([]string{keyGreeting}, nil, func(context.Context, *config.Resolver) error { return errors.New("the sink could not be built") })
	res, err := svc.Set(context.Background(), admin, setArgs(keyGreeting, "x"))
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != ops.WarnApplyFailed {
		t.Fatalf("Set = %+v, %v", res, err)
	}
	if r := resolve(t, svc, keyGreeting); r.Value != "x" {
		t.Fatalf("the value was not stored: %+v", r)
	}
}

// A change that names the revision it was based on is refused once the
// stored value changed (a stale settings page); nothing is stored.
func TestStaleRevisionIsRefused(t *testing.T) {
	f := newFixture(t)
	svc := f.txService()
	if _, err := svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyGreeting, Value: "old"})); err != nil {
		t.Fatal(err)
	}
	rev := listByKey(t, svc)[keyGreeting].Revision
	if rev == "" || rev == "none" {
		t.Fatalf("revision %q", rev)
	}
	if _, err := svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyGreeting, Value: "new"}, ops.SettingChange{Key: keyToken, Value: "t2"})); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyGreeting, Value: "old", Revision: rev}))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeConflict || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("err = %v", err)
	}
	if r := resolve(t, svc, keyGreeting); r.Value != "new" {
		t.Fatalf("a stale change was stored: %+v", r)
	}
	cur := listByKey(t, svc)[keyGreeting].Revision
	if _, err := svc.Set(context.Background(), admin, batchArgs(ops.SettingChange{Key: keyGreeting, Value: "newer", Revision: cur})); err != nil {
		t.Fatalf("current revision refused: %v", err)
	}
	if listByKey(t, svc)[keyMode].Revision != "none" {
		t.Fatal("a key without a panel value has revision none")
	}
}
