package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

// Names used by the seed.
const (
	// SeedMarker is the auth_meta key that records the seed.
	SeedMarker = "admin_seed"
	// AuditActionSeed is the audit action of the seed.
	AuditActionSeed = "user.seed"
	// SeedActor is the audit actor of the seed: nobody is signed in yet.
	SeedActor = "system"

	// MaxUsernameLen is the longest user name in characters.
	MaxUsernameLen = 64
)

// SeedStore is the persistence port of the [Seeder]; *store.Store implements it.
type SeedStore interface {
	HasUsers(ctx context.Context) (bool, error)
	SeedAdmin(ctx context.Context, p store.SeedAdminParams) (bool, error)
}

var _ SeedStore = (*store.Store)(nil)

// SeedConfig is the seed input: MINION_ADMIN_USERNAME and
// MINION_ADMIN_PASSWORD (or _FILE) after resolution.
type SeedConfig struct {
	// Username defaults to [config.DefaultAdminUsername].
	Username string
	// Password is the configured password. Empty means "generate one".
	Password logx.Secret
}

// SeedResult says what [Seeder.Seed] did.
type SeedResult struct {
	// Created is true when this call created the admin account.
	Created bool
	// Generated is true when the password was generated (and logged once).
	Generated bool
	// UserID is the id of the created account; empty when Created is false.
	UserID string
}

// Seeder creates the first admin account (AD-10).
type Seeder struct {
	Store  SeedStore
	Hasher *Hasher
	// Log receives the seed messages. The generated password is written to it
	// once, as the only password that is ever logged (AD-20).
	Log *slog.Logger
	// Now defaults to time.Now; Random to crypto/rand.
	Now    func() time.Time
	Random io.Reader
}

// ErrInvalidUsername is returned for a seed user name that cannot be used.
var ErrInvalidUsername = errors.New("local: invalid user name")

// ValidateUsername checks a user name: 1 to [MaxUsernameLen] characters of
// valid UTF-8, no control characters, no leading or trailing space.
func ValidateUsername(name string) error {
	switch {
	case name == "" || utf8.RuneCountInString(name) > MaxUsernameLen:
		return fmt.Errorf("%w: it must have 1 to %d characters", ErrInvalidUsername, MaxUsernameLen)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: it is not valid UTF-8", ErrInvalidUsername)
	case name != strings.TrimSpace(name):
		return fmt.Errorf("%w: it has leading or trailing space", ErrInvalidUsername)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: it contains a control character", ErrInvalidUsername)
		}
	}
	return nil
}

// Seed creates the local admin when, and only when, the users table is empty.
// The account, its admin grant, the seed marker and an audit row are written
// in one transaction. On every later start it does nothing; in particular it
// never resets the password of an existing account.
//
// With a configured password the account is a normal one. Without one a random
// password is generated, logged once at error level, so that no log threshold
// hides it (the one named exception to "passwords are never logged", AD-20)
// and the account is flagged must_change_password. The configured password is never logged.
func (s *Seeder) Seed(ctx context.Context, cfg SeedConfig) (SeedResult, error) {
	has, err := s.Store.HasUsers(ctx)
	if err != nil {
		return SeedResult{}, fmt.Errorf("admin seed: %w", err)
	}
	if has {
		return SeedResult{}, nil
	}

	username := cfg.Username
	if username == "" {
		username = config.DefaultAdminUsername
	}
	if err := ValidateUsername(username); err != nil {
		return SeedResult{}, fmt.Errorf("admin seed: MINION_ADMIN_USERNAME: %w", err)
	}

	password := cfg.Password.Reveal()
	generated := password == ""
	if generated {
		if password, err = GeneratePassword(s.Random); err != nil {
			return SeedResult{}, fmt.Errorf("admin seed: %w", err)
		}
	}
	hash, err := s.Hasher.Hash(ctx, password)
	if err != nil {
		return SeedResult{}, fmt.Errorf("admin seed: %w", err)
	}

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ts := store.UnixMS(now())
	id := store.NewID()
	ev := audit.Event{Actor: SeedActor, Action: AuditActionSeed, Target: id, Outcome: "success"}
	if err := ev.Validate(); err != nil {
		return SeedResult{}, fmt.Errorf("admin seed: %w", err)
	}
	created, err := s.Store.SeedAdmin(ctx, store.SeedAdminParams{
		User: store.UserRow{
			ID: id, Username: username, PasswordHash: hash,
			MustChangePassword: generated, CredentialVersion: 1, Active: true,
			CreatedAt: ts, UpdatedAt: ts,
		},
		Marker: SeedMarker,
		Audit:  &store.AuditRow{ID: store.NewID(), TS: ts, Actor: ev.Actor, Action: ev.Action, Target: ev.Target, Outcome: ev.Outcome},
	})
	if err != nil {
		return SeedResult{}, fmt.Errorf("admin seed: %w", err)
	}
	if !created { // another Hub process seeded first
		return SeedResult{}, nil
	}

	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	if generated {
		// The only place that logs a password (AD-20): the generated initial
		// admin password, printed once for the human, marked must-change. It
		// is logged at error level on purpose: the account is already
		// committed, so a record that a threshold of "error" hides would lock
		// the operator out for good (a restart never reseeds). Every
		// supported MINION_LOG_LEVEL lets it through.
		log.Log(ctx, slog.LevelError, "initial admin account created with a generated password; sign in and change it, it is not shown again",
			"username", username, "password", password, "must_change_password", true)
	} else {
		log.Info("initial admin account created from the configured password", "username", username)
	}
	return SeedResult{Created: true, Generated: generated, UserID: id}, nil
}
