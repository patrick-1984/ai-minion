package truenas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// The exchange with the Hub uses two files in two directories:
//
//   - RequestFile (Config.RequestFile): written by the Hub (or, for the first
//     legacy upgrade, by the owner) into a directory the Hub may write. The
//     executor reads it as untrusted input: opened with O_NOFOLLOW and
//     O_NONBLOCK, it must be a regular file of at most MaxRequestSize bytes,
//     strict JSON, "version": 1.
//   - StatusFile (Config.StatusFile): written by the executor into a directory
//     the Hub only reads, atomically, with sanitized content (codes, tags,
//     digests, timestamps; never configuration values or raw error text).

// MaxRequestSize bounds the request file.
const MaxRequestSize = 16 << 10

// RequestVersion and StatusVersion are the schema versions.
const (
	RequestVersion = 1
	StatusVersion  = 1
)

// Request is the content of the request file.
type Request struct {
	Version int     `json:"version"`
	Policy  *Policy `json:"policy,omitempty"`
	Once    *Once   `json:"once,omitempty"`
}

// Policy is the update policy the Hub exports. It is honoured only when the
// config sets allow_hub_policy.
type Policy struct {
	Enabled  bool   `json:"enabled"`
	Channel  string `json:"channel"`
	Window   string `json:"window"`
	Revision int64  `json:"revision"`
}

// Once is a one-shot request: update from Baseline to exactly Target, once,
// before ExpiresAt. It is how the first upgrade of a legacy exact-tag app is
// authorized (issued by the owner on the host) and how "Update now" will work.
type Once struct {
	ID string `json:"id"`
	// Baseline is the release the app runs now; Target the release to install.
	Baseline string `json:"baseline"`
	Target   string `json:"target"`
	// PolicyRevision binds the request to a policy revision (0 = unbound).
	PolicyRevision int64     `json:"policy_revision,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

var onceIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// MaxOnceLifetime is the longest a one-shot request may be valid ahead.
const MaxOnceLifetime = 7 * 24 * time.Hour

// ReadRequest reads and validates the request file. A missing file is
// (nil, nil). Every problem is an error with a short code in its text.
func ReadRequest(path string, now time.Time) (*Request, error) {
	if path == "" {
		return nil, nil
	}
	f, err := openNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("request_unreadable")
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("request_not_regular_file")
	}
	if fi.Size() > MaxRequestSize {
		return nil, errors.New("request_too_large")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxRequestSize+1))
	if err != nil {
		return nil, errors.New("request_unreadable")
	}
	if len(data) > MaxRequestSize {
		return nil, errors.New("request_too_large")
	}
	var r Request
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, errors.New("request_invalid_json")
	}
	if dec.More() {
		return nil, errors.New("request_invalid_json")
	}
	if r.Version != RequestVersion {
		return nil, errors.New("request_unsupported_version")
	}
	if r.Policy != nil {
		if _, err := ParseWindow(r.Policy.Window); err != nil {
			return nil, errors.New("request_invalid_policy")
		}
		if _, err := ParseChannel(r.Policy.Channel); err != nil {
			return nil, errors.New("request_invalid_policy")
		}
	}
	if o := r.Once; o != nil {
		if !onceIDRE.MatchString(o.ID) {
			return nil, errors.New("request_invalid_once")
		}
		for _, tag := range []string{o.Baseline, o.Target} {
			if v, err := version.ParseVersion(tag); err != nil || v.String() != tag {
				return nil, errors.New("request_invalid_once")
			}
		}
		if !o.ExpiresAt.After(now) {
			return nil, errors.New("request_expired")
		}
		if o.ExpiresAt.After(now.Add(MaxOnceLifetime)) {
			return nil, errors.New("request_invalid_once")
		}
		if o.PolicyRevision != 0 && (r.Policy == nil || r.Policy.Revision != o.PolicyRevision) {
			return nil, errors.New("request_policy_revision_mismatch")
		}
	}
	return &r, nil
}

// Status is the sanitized content of the status file.
type Status struct {
	Version   int       `json:"version"`
	App       string    `json:"app"`
	UpdatedAt time.Time `json:"updated_at"`
	// Tick is a short code for what the last run did: idle, outside_window,
	// disabled, hub_down, up_to_date, updated, rolled_back, reconciled,
	// recovery_required, busy, refused.
	Tick string `json:"tick"`
	// Detail is a code (never raw error text).
	Detail         string         `json:"detail,omitempty"`
	CurrentTag     string         `json:"current_tag,omitempty"`
	Window         string         `json:"window"`
	Channel        string         `json:"channel"`
	Enabled        bool           `json:"enabled"`
	Quarantined    []string       `json:"quarantined,omitempty"`
	LastAttempt    *AttemptStatus `json:"last_attempt,omitempty"`
	RequestError   string         `json:"request_error,omitempty"`
	RequestHandled string         `json:"request_handled,omitempty"`
}

// AttemptStatus is the public part of an attempt.
type AttemptStatus struct {
	ID         string    `json:"id"`
	Phase      string    `json:"phase"`
	Reason     string    `json:"reason,omitempty"`
	FromTag    string    `json:"from_tag"`
	ToTag      string    `json:"to_tag"`
	Digest     string    `json:"digest"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Terminal   bool      `json:"terminal"`
	Outcome    string    `json:"outcome,omitempty"`
	NeedsHuman bool      `json:"needs_human,omitempty"`
}

func attemptStatus(a *Attempt) *AttemptStatus {
	if a == nil {
		return nil
	}
	s := &AttemptStatus{ID: a.ID, Phase: a.Phase, Reason: a.Reason, FromTag: a.FromTag, ToTag: a.ToTag,
		Digest: a.CandidateDigest, StartedAt: a.StartedAt, UpdatedAt: a.UpdatedAt, Terminal: Terminal(a.Phase)}
	if s.Terminal {
		s.Outcome = a.Phase
	}
	s.NeedsHuman = a.Phase == PhaseRecoveryRequired
	return s
}

// WriteStatus writes the status file atomically (mode 0644: the Hub reads it
// through a read-only mount of its directory).
func WriteStatus(path string, s Status) error {
	if path == "" {
		return nil
	}
	s.Version = StatusVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("status: %w", err)
	}
	return writeFileAtomic(path, append(data, '\n'), 0o644)
}
