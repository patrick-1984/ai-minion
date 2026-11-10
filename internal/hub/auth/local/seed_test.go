package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newSeeder(t *testing.T, st SeedStore) (*Seeder, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	return &Seeder{
		Store:  st,
		Hasher: fastHasher(),
		Log:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, logs
}

func TestSeedWithConfiguredPassword(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sd, logs := newSeeder(t, st)

	res, err := sd.Seed(ctx, SeedConfig{Username: "root", Password: logx.Secret("hunter2-hunter2")})
	if err != nil || !res.Created || res.Generated || res.UserID == "" {
		t.Fatalf("Seed = %+v, %v", res, err)
	}
	u, err := st.UserByID(ctx, res.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "root" || u.Role != "admin" || !u.Active || u.MustChangePassword || u.CredentialVersion != 1 {
		t.Errorf("seeded user = %+v", u)
	}
	if ok, _, _ := fastHasher().Verify(ctx, "hunter2-hunter2", u.PasswordHash); !ok {
		t.Error("the stored hash does not verify the configured password")
	}
	if strings.Contains(logs.String(), "hunter2") || strings.Contains(logs.String(), u.PasswordHash) {
		t.Errorf("the configured password or its hash was logged:\n%s", logs)
	}
	// Seed marker and audit row exist.
	rows, err := st.ListAudit(ctx, store.AuditQuery{Limit: 5})
	if err != nil || len(rows) != 1 || rows[0].Action != AuditActionSeed || rows[0].Actor != SeedActor || rows[0].Target != u.ID {
		t.Errorf("audit rows = %+v, %v", rows, err)
	}
	for _, r := range rows {
		if strings.Contains(r.Actor+r.Action+r.Target+r.Outcome, "hunter2") {
			t.Error("the password is in the audit row")
		}
	}
}

func TestSeedRunsOnceAndNeverResetsAPassword(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sd, logs := newSeeder(t, st)
	first, err := sd.Seed(ctx, SeedConfig{Username: "root", Password: logx.Secret("first-password")})
	if err != nil || !first.Created {
		t.Fatalf("first Seed = %+v, %v", first, err)
	}
	before, _ := st.UserByID(ctx, first.UserID)

	// A later start with another password, another name, or none at all.
	for _, cfg := range []SeedConfig{
		{Username: "root", Password: logx.Secret("second-password")},
		{Username: "other", Password: logx.Secret("second-password")},
		{Username: "root"},
		{},
	} {
		logs.b.Reset()
		res, err := sd.Seed(ctx, cfg)
		if err != nil || res.Created || res.UserID != "" {
			t.Fatalf("later Seed(%q) = %+v, %v", cfg.Username, res, err)
		}
		after, err := st.UserByID(ctx, first.UserID)
		if err != nil || after != before {
			t.Fatalf("the account changed: %+v -> %+v (%v)", before, after, err)
		}
		if logs.String() != "" {
			t.Errorf("a no-op seed logged: %s", logs)
		}
	}
	if _, err := st.UserByIdentifier(ctx, "other"); !errors.Is(err, store.ErrNotFound) {
		t.Error("a second admin was created")
	}
	rows, _ := st.ListAudit(ctx, store.AuditQuery{Limit: 10})
	if len(rows) != 1 {
		t.Errorf("%d audit rows after repeated seeds, want 1", len(rows))
	}
}

func TestSeedWithAMarkerAndNoUsersCreatesNothingAndLogsNothing(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if _, err := st.DB().Exec(`INSERT INTO auth_meta (key, value, created_at) VALUES (?, 'earlier-user', 500)`, SeedMarker); err != nil {
		t.Fatal(err)
	}
	sd, logs := newSeeder(t, st)
	res, err := sd.Seed(ctx, SeedConfig{})
	if err != nil || res.Created || res.Generated || res.UserID != "" {
		t.Fatalf("Seed = %+v, %v; want a no-op", res, err)
	}
	if logs.String() != "" || strings.Contains(logs.String(), "password=") {
		t.Errorf("a no-op seed logged: %s", logs)
	}
	if has, err := st.HasUsers(ctx); err != nil || has {
		t.Errorf("HasUsers = %v, %v; want false", has, err)
	}
	if rows, err := st.ListAudit(ctx, store.AuditQuery{Limit: 5}); err != nil || len(rows) != 0 {
		t.Errorf("audit rows = %+v, %v", rows, err)
	}
}

func TestSeedWithoutPasswordGeneratesOneAndFlagsMustChange(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	logs := &syncBuf{}
	sd := &Seeder{Store: st, Hasher: fastHasher(), Log: slog.New(slog.NewJSONHandler(logs, nil))}

	res, err := sd.Seed(ctx, SeedConfig{})
	if err != nil || !res.Created || !res.Generated {
		t.Fatalf("Seed = %+v, %v", res, err)
	}
	u, _ := st.UserByID(ctx, res.UserID)
	if u.Username != "admin" || !u.MustChangePassword || u.Role != "admin" {
		t.Errorf("seeded user = %+v", u)
	}

	// The log carries exactly one line with the password, marked must-change.
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want 1:\n%s", len(lines), logs)
	}
	var rec struct {
		Level, Msg, Username, Password string
		MustChange                     bool `json:"must_change_password"`
	}
	if err := jsonUnmarshal(lines[0], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Username != "admin" || !rec.MustChange || rec.Level != "ERROR" || len(rec.Password) != 24 {
		t.Errorf("log record = %+v", rec)
	}
	if ok, _, _ := fastHasher().Verify(ctx, rec.Password, u.PasswordHash); !ok {
		t.Error("the logged password does not open the account")
	}
	if strings.Contains(logs.String(), u.PasswordHash) {
		t.Error("the hash was logged")
	}

	// A second start prints nothing.
	logs.b.Reset()
	if res, _ := sd.Seed(ctx, SeedConfig{}); res.Created || logs.String() != "" {
		t.Errorf("second start: %+v, log %q", res, logs)
	}
}

func TestSeedValidatesTheUsername(t *testing.T) {
	st := openStore(t)
	sd, _ := newSeeder(t, st)
	for _, name := range []string{" admin", "admin ", "a\nb", strings.Repeat("x", 65), "\xff"} {
		_, err := sd.Seed(context.Background(), SeedConfig{Username: name, Password: "pw-pw-pw-pw"})
		if !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("username %q: err = %v", name, err)
		}
	}
	if has, _ := st.HasUsers(context.Background()); has {
		t.Error("a user was created for an invalid name")
	}
}

type failingSeedStore struct{ SeedStore }

func (failingSeedStore) SeedAdmin(context.Context, store.SeedAdminParams) (bool, error) {
	return false, errors.New("disk full")
}

func TestSeedFailureIsReportedAndNothingIsPrinted(t *testing.T) {
	sd, logs := newSeeder(t, failingSeedStore{openStore(t)})
	if _, err := sd.Seed(context.Background(), SeedConfig{}); err == nil {
		t.Fatal("a failed seed returned no error")
	}
	if logs.String() != "" {
		t.Errorf("a generated password was printed although the account was not created:\n%s", logs)
	}
}

type racingSeedStore struct{ SeedStore }

func (racingSeedStore) SeedAdmin(context.Context, store.SeedAdminParams) (bool, error) {
	return false, nil // someone else seeded between the check and the transaction
}

func TestSeedLosingTheRaceDoesNotPrintAPassword(t *testing.T) {
	sd, logs := newSeeder(t, racingSeedStore{openStore(t)})
	res, err := sd.Seed(context.Background(), SeedConfig{})
	if err != nil || res.Created {
		t.Fatalf("Seed = %+v, %v", res, err)
	}
	if logs.String() != "" {
		t.Errorf("a password was printed for an account this process did not create:\n%s", logs)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// TestGeneratedPasswordSurvivesTheErrorLogLevel: the account is committed
// before the password is logged and a restart never reseeds, so a log
// threshold of "error" (MINION_LOG_LEVEL=error) must not hide the password.
func TestGeneratedPasswordSurvivesTheErrorLogLevel(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	logs := &syncBuf{}
	sd := &Seeder{Store: st, Hasher: fastHasher(), Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))}
	res, err := sd.Seed(ctx, SeedConfig{})
	if err != nil || !res.Created || !res.Generated {
		t.Fatalf("Seed = %+v, %v", res, err)
	}
	u, _ := st.UserByID(ctx, res.UserID)
	var pw string
	for _, f := range strings.Fields(logs.String()) {
		if v, ok := strings.CutPrefix(f, "password="); ok {
			pw = v
		}
	}
	if len(pw) != 24 {
		t.Fatalf("the generated password is not in the error-level log:\n%s", logs)
	}
	if ok, _, _ := fastHasher().Verify(ctx, pw, u.PasswordHash); !ok {
		t.Error("the logged password does not open the account")
	}
}
