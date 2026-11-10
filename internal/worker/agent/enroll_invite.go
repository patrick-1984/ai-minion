package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Invitation enrollment on the Worker (Story 4.6, AD-7, AD-6).
//
// The order of the steps is the contract:
//
//  1. the Worker key pair is saved (LoadOrCreateKey), so that a retry after a
//     crash presents the same identity and the Hub answers idempotently;
//  2. the Worker connects in enroll_invite mode and checks the Hub's identity
//     against the fingerprint inside the invitation key; only then it sends the
//     secret, inside the encrypted channel;
//  3. the Hub's welcome is committed together with the pin to worker.db;
//  4. only then enrollment is reported as a success.
//
// The package keeps no copy of the invitation key after Enroll returns.

// HookEnrollInvite is the name of the pre-start hook that enrolls with the
// invitation key of the run (if there is one).
const HookEnrollInvite = "enroll_invite"

// Defaults of the enrollment retry: a Hub that is briefly unreachable at
// start must not make provisioning fail at once.
const (
	// DefaultInviteAttempts is the number of tries of one enrollment run.
	DefaultInviteAttempts = 3
	// DefaultInviteRetryWait is the wait before the second try; it doubles.
	DefaultInviteRetryWait = 2 * time.Second
)

const alreadyEnrolledMessage = "invitation key ignored: this Worker is already enrolled (enrollment is permanent until `minion worker reset`)"

// configLockName is the lock file in the state directory that serializes the
// config commands of every process (`minion worker config ...`); the cleanup of
// the stored invitation key takes the same lock.
const configLockName = ".config.lock"

// enrollLockName is the lock file in the state directory that serializes the
// enrollment of every process that starts on the same state directory: it is
// held from the first check of the enrollment through the creation of the
// Worker key and the commit.
const enrollLockName = ".enroll.lock"

// enrollLockPoll is how often a waiting process tries the lock again.
const enrollLockPoll = 50 * time.Millisecond

// FingerprintMismatchError is returned when the Hub's identity is not the one
// named by the invitation key. The secret was never sent.
type FingerprintMismatchError struct {
	// Want is the fingerprint inside the invitation key.
	Want identity.Fingerprint
	// Got is the fingerprint of the key the Hub presented; empty when it could
	// not be determined.
	Got identity.Fingerprint
}

func (e *FingerprintMismatchError) Error() string {
	got := string(e.Got)
	if got == "" {
		got = "(unknown)"
	}
	return fmt.Sprintf("the Hub's identity does not match the invitation key: the key names the Hub %s but the Hub at "+
		"this address is %s; the invitation secret was not sent (check the Hub URL and the key)", e.Want, got)
}

// Unwrap lets errors.Is match [transport.ErrFingerprintMismatch].
func (e *FingerprintMismatchError) Unwrap() error { return transport.ErrFingerprintMismatch }

// InviteParams is what one enrollment attempt needs.
type InviteParams struct {
	// HubURL is the configured Hub URL (any of https, http, wss, ws).
	HubURL string
	// Key is the Worker's identity key, already saved.
	Key *identity.Key
	// Name is the Worker's display name for the hello.
	Name string
	// Ops are the operations the Worker announces; empty is fine, the Worker
	// announces its ops again when it connects for real.
	Ops []string
	// Protocols is the supported protocol range; zero means proto.Current.
	Protocols proto.Range
	// Invite is the invitation key.
	Invite identity.InviteKey
}

// InviteOutcome is what a successful enrollment attempt returns.
type InviteOutcome struct {
	// Pin is the Hub key to pin: authenticated by the invitation fingerprint.
	Pin identity.Pin
	// Welcome is the Hub's welcome.
	Welcome HubWelcome
}

// InviteDialer is the transport port of invitation enrollment: it dials the
// Hub, runs the enroll_invite handshake, sends the secret and returns the
// welcome. The production implementation is [NewInviteDialer]; tests use a
// fake, for example to cut the connection at a chosen step.
type InviteDialer interface {
	Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error)
}

// InviteState is the persistence port of invitation enrollment; *Enrollments
// implements it.
type InviteState interface {
	LoadOrCreateKey() (*identity.Key, error)
	Load(ctx context.Context) (Enrollment, bool, error)
	Commit(ctx context.Context, e Enrollment) error
}

var _ InviteState = (*Enrollments)(nil)

// InviteEnrollerConfig configures [NewInviteEnroller].
type InviteEnrollerConfig struct {
	// State persists the key pair and the enrollment.
	State InviteState
	// Dialer talks to the Hub; the default is [NewInviteDialer].
	Dialer InviteDialer
	// HubURL is the Hub to enroll at.
	HubURL string
	// Name is the Worker's display name.
	Name string
	// Protocols is the supported protocol range; zero means proto.Current.
	Protocols proto.Range
	// Attempts and RetryWait tune the retry of a transient failure (an
	// unreachable Hub, a cut connection); zero means the defaults. A refusal
	// of the Hub or of its identity is never retried.
	Attempts  int
	RetryWait time.Duration
	// Sleep waits between attempts; nil means a context-aware timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Logger receives operational messages, never the key.
	Logger *slog.Logger
}

// InviteEnroller enrolls a Worker with an invitation key.
type InviteEnroller struct {
	cfg InviteEnrollerConfig
	log *slog.Logger
}

// NewInviteEnroller validates cfg and returns the enroller.
func NewInviteEnroller(cfg InviteEnrollerConfig) (*InviteEnroller, error) {
	if cfg.State == nil {
		return nil, errors.New("agent: invitation enrollment needs a state")
	}
	if cfg.Dialer == nil {
		cfg.Dialer = NewInviteDialer()
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = DefaultInviteAttempts
	}
	if cfg.RetryWait <= 0 {
		cfg.RetryWait = DefaultInviteRetryWait
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &InviteEnroller{cfg: cfg, log: log}, nil
}

// InviteResult is the outcome of [InviteEnroller.Enroll].
type InviteResult struct {
	// Enrollment is the stored enrollment.
	Enrollment Enrollment
	// AlreadyEnrolled is true when the Worker was enrolled before: the
	// invitation key was ignored and nothing was sent.
	AlreadyEnrolled bool
}

// Enroll runs the steps described at the top of this file. An already
// enrolled Worker ignores the key: enrollment is permanent until reset (AD-9).
func (e *InviteEnroller) Enroll(ctx context.Context, invite identity.InviteKey) (InviteResult, error) {
	cur, ok, err := e.cfg.State.Load(ctx)
	if err != nil {
		return InviteResult{}, fmt.Errorf("read the enrollment: %w", err)
	}
	if ok {
		e.log.Info(alreadyEnrolledMessage)
		return InviteResult{Enrollment: cur, AlreadyEnrolled: true}, nil
	}
	if strings.TrimSpace(e.cfg.HubURL) == "" {
		return InviteResult{}, errors.New("an invitation key needs the Hub URL: give --server https://<hub>")
	}
	if err := validateHubURL(e.cfg.HubURL); err != nil {
		return InviteResult{}, err
	}
	if _, err := identity.ParseFingerprint(string(invite.Fingerprint)); err != nil || len(invite.Secret) == 0 {
		return InviteResult{}, errors.New("the invitation key is malformed")
	}

	// 1. The key pair first: a retry after any later failure is the same
	// Worker, so the Hub can answer idempotently.
	key, err := e.cfg.State.LoadOrCreateKey()
	if err != nil {
		return InviteResult{}, fmt.Errorf("save the Worker key: %w", err)
	}

	// 2. Connect, verify the Hub, send the secret.
	out, err := e.dialWithRetry(ctx, InviteParams{
		HubURL: e.cfg.HubURL, Key: key, Name: e.cfg.Name, Protocols: e.cfg.Protocols, Invite: invite,
	})
	if err != nil {
		return InviteResult{}, err
	}
	if out.Welcome.WorkerID == "" || out.Welcome.Name == "" {
		return InviteResult{}, errors.New("the Hub's welcome is incomplete: it names no Worker")
	}

	// 3. Pin and enrollment together, before success is reported.
	enr := Enrollment{
		Pin:      out.Pin,
		WorkerID: out.Welcome.WorkerID,
		Name:     out.Welcome.Name,
		Owner:    out.Welcome.Owner,
		Kind:     out.Welcome.Kind,
		Policy:   out.Welcome.Policy,
		// The Hub this key was enrolled at: a later identity replacement
		// revokes it there, whatever the configuration says by then.
		HubURL: e.cfg.HubURL,
	}
	if err := e.cfg.State.Commit(ctx, enr); err != nil {
		return InviteResult{}, fmt.Errorf("store the enrollment: %w", err)
	}
	e.log.Info("enrolled with an invitation key", "worker_id", enr.WorkerID, "owner", enr.Owner, "kind", enr.Kind)
	return InviteResult{Enrollment: enr}, nil
}

func (e *InviteEnroller) dialWithRetry(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	wait := e.cfg.RetryWait
	var err error
	for attempt := 1; ; attempt++ {
		var out *InviteOutcome
		out, err = e.cfg.Dialer.Enroll(ctx, p)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if permanentInviteError(err) || attempt >= e.cfg.Attempts {
			return nil, err
		}
		e.log.Info("enrollment attempt failed, retrying", "attempt", attempt, "error", err.Error(), "retry_in", wait.String())
		if serr := e.cfg.Sleep(ctx, wait); serr != nil {
			return nil, serr
		}
		wait *= 2
	}
}

// permanentInviteError reports whether retrying cannot help: the Hub refused
// the key, the Hub is not the one the key names, or the handshake was
// rejected.
func permanentInviteError(err error) bool {
	var pe *proto.Error
	var fm *FingerprintMismatchError
	return errors.As(err, &pe) || errors.As(err, &fm) ||
		errors.Is(err, transport.ErrFingerprintMismatch) || errors.Is(err, transport.ErrPinMismatch) ||
		errors.Is(err, transport.ErrBadSignature) || errors.Is(err, transport.ErrChainInvalid) ||
		errors.Is(err, transport.ErrProtocol) || errors.Is(err, transport.ErrConfig)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ------------------------------------------------------------ production dialer

// NewInviteDialer returns the production [InviteDialer]: WSS dial (any
// certificate is accepted, A-15), AD-6 handshake in enroll_invite mode, which
// verifies the Hub against the fingerprint of the key before anything secret
// is sent, then the invite.secret frame inside the channel and the Hub's
// answer. The connection is closed afterwards: the connector reconnects in
// connect mode with the new pin.
func NewInviteDialer() InviteDialer {
	return inviteDialer{dialTimeout: DefaultDialTimeout, handshakeTimeout: handshakeTimeout}
}

type inviteDialer struct {
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
}

// inviteSecretFrame is the frame of the enroll_invite mode (AD-7). The Hub
// accepts the bare secret.
type inviteSecretFrame struct {
	Type   string `json:"type"`
	Secret string `json:"secret"`
}

func (d inviteDialer) Enroll(ctx context.Context, p InviteParams) (*InviteOutcome, error) {
	if err := validateHubURL(p.HubURL); err != nil {
		return nil, err
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, d.dialTimeout)
	raw, err := transport.Dial(dialCtx, p.HubURL)
	// The cause is read before the cancel: after cancelDial the context
	// always reports "canceled".
	dialErr := dialCtx.Err()
	cancelDial()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(dialErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("dial the hub: no answer within %s", d.dialTimeout)
		}
		return nil, fmt.Errorf("dial the hub: %w", err)
	}
	conn := &helloTap{Conn: raw}
	hsTimer := time.AfterFunc(d.handshakeTimeout, func() { _ = conn.Close() })
	defer hsTimer.Stop()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	res, err := transport.WorkerHandshake(conn, transport.WorkerConfig{
		Key:            p.Key,
		Name:           p.Name,
		Ops:            p.Ops,
		Protocols:      p.Protocols,
		Mode:           transport.ModeEnrollInvite,
		HubFingerprint: p.Invite.Fingerprint,
	})
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, transport.ErrFingerprintMismatch) {
			return nil, &FingerprintMismatchError{Want: p.Invite.Fingerprint, Got: conn.hubFingerprint()}
		}
		return nil, err
	}
	defer func() { _ = res.Channel.Close() }()
	stopCh := context.AfterFunc(ctx, func() { _ = res.Channel.Close() })
	stop()
	defer stopCh()

	// The Hub is authenticated: the secret goes out now, inside the channel.
	secret := inviteSecretPart(p.Invite)
	if err := proto.WriteFrame(res.Channel, proto.StreamFrameLimit,
		inviteSecretFrame{Type: proto.TypeInviteSecret, Secret: secret}); err != nil {
		return nil, fmt.Errorf("send the invitation secret: %w", err)
	}
	reply, err := readReply(res.Channel)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	switch reply.Type {
	case proto.TypeWelcome:
		return &InviteOutcome{Pin: res.Pin, Welcome: reply.Welcome}, nil
	case proto.TypeRevoke:
		return nil, errors.New("the Hub has removed this Worker's identity; it cannot enroll again with this key pair " +
			"(remove the Worker key in the state directory to start over)")
	}
	return nil, fmt.Errorf("the Hub answered the invitation with %q instead of a welcome", reply.Type)
}

// hubURLError is an unusable Hub URL. It is permanent: retrying cannot help.
type hubURLError struct{ err error }

func (e *hubURLError) Error() string   { return e.err.Error() }
func (e *hubURLError) Unwrap() []error { return []error{e.err, transport.ErrConfig} }

// validateHubURL checks the Hub URL the way the dial does, so that a bad
// scheme or host fails at once and is not retried as a transient failure.
func validateHubURL(hubURL string) error {
	if _, err := transport.ConnectURL(hubURL); err != nil {
		return &hubURLError{err: err}
	}
	return nil
}

// inviteSecretPart returns the secret of the key in its wire form: the part of
// mnk_<fingerprint>.<secret> after the dot.
func inviteSecretPart(k identity.InviteKey) string {
	_, secret, _ := strings.Cut(k.String(), ".")
	return secret
}

// helloTap passes reads through and remembers the Hub's key from the Hub's
// hello frame, the first frame the Hub sends. The handshake reports a
// fingerprint mismatch without the Hub key, and the error should name both
// fingerprints.
type helloTap struct {
	net.Conn

	mu     sync.Mutex
	buf    []byte
	done   bool
	hubPub ed25519.PublicKey
}

func (t *helloTap) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.feed(p[:n])
	}
	return n, err
}

func (t *helloTap) feed(b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.buf = append(t.buf, b...)
	if len(t.buf) < 4 {
		return
	}
	n := int(uint32(t.buf[0])<<24 | uint32(t.buf[1])<<16 | uint32(t.buf[2])<<8 | uint32(t.buf[3]))
	if n > proto.HandshakeFrameLimit {
		t.done, t.buf = true, nil
		return
	}
	if len(t.buf) < 4+n {
		return
	}
	var h struct {
		HubPub identity.B64 `json:"hub_pub"`
	}
	if json.Unmarshal(t.buf[4:4+n], &h) == nil && len(h.HubPub) == ed25519.PublicKeySize {
		t.hubPub = ed25519.PublicKey(h.HubPub)
	}
	t.done, t.buf = true, nil
}

func (t *helloTap) hubFingerprint() identity.Fingerprint {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hubPub == nil {
		return ""
	}
	f, err := identity.FingerprintOf(t.hubPub)
	if err != nil {
		return ""
	}
	return f
}

// ------------------------------------------------------------ pre-start hook

// inviteHookDeps are the seams of the pre-start hook that tests replace.
type inviteHookDeps struct {
	// stdin is read for --invite-key -.
	stdin io.Reader
	// stderr receives the one warning line of the literal form.
	stderr io.Writer
	// openState opens the Worker state; the returned func closes it.
	openState func(ctx context.Context, stateDir string) (InviteState, func() error, error)
	// dialer talks to the Hub; nil means the production dialer.
	dialer InviteDialer
	// sleep, attempts and wait tune the retry; zero values mean defaults.
	sleep    func(ctx context.Context, d time.Duration) error
	attempts int
	wait     time.Duration
}

func defaultInviteHookDeps() inviteHookDeps {
	return inviteHookDeps{
		stdin:  os.Stdin,
		stderr: os.Stderr,
		openState: func(ctx context.Context, stateDir string) (InviteState, func() error, error) {
			st, err := store.OpenContext(ctx, stateDir)
			if err != nil {
				return nil, nil, err
			}
			return NewEnrollments(st, stateDir), st.Close, nil
		},
	}
}

func init() { RegisterPreStart(HookEnrollInvite, defaultInviteHookDeps().run) }

// storedConfig is the part of the Worker config file that the hook uses to
// remove a stored invitation key; *config.ConfigFile implements it.
type storedConfig interface {
	Lookup(key string) (string, bool, error)
	Delete(key string)
	Save() error
	Path() string
}

// run is the pre-start hook. Without an invitation key it does nothing. An
// enrolled Worker ignores a key with an info line, without reading the key's
// file or stdin and without parsing it (enrollment is permanent, AD-9). Only
// an unenrolled Worker reads the key, prints the warning of the literal form,
// enrolls and reports "enrolled as <name>, owner <user>" to s.Out after the
// enrollment is stored.
func (d inviteHookDeps) run(ctx context.Context, s Settings) error {
	if s.Resolver == nil {
		return nil
	}
	given, literal, err := config.InviteKeyGiven(s.Resolver)
	if err != nil {
		return err
	}
	if !given {
		return nil
	}
	if literal && d.stderr != nil && !s.InviteArgvWarned {
		_, _ = fmt.Fprintln(d.stderr, config.InviteKeyArgvWarning)
	}

	// Two processes that start on one state directory must not both create a
	// Worker key: the later save would replace the key the first one already
	// presented with the one-time invitation. The lock covers the check of
	// the enrollment, the key creation and the commit, and is taken before
	// the Worker state is opened.
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return fmt.Errorf("create the state directory %s: %w", s.StateDir, err)
	}
	releaseEnroll, err := lockEnrollment(ctx, filepath.Join(s.StateDir, enrollLockName))
	if err != nil {
		return err
	}
	defer releaseEnroll()

	state, closeState, err := d.openState(ctx, s.StateDir)
	if err != nil {
		return fmt.Errorf("open the Worker state in %s: %w", s.StateDir, err)
	}
	defer func() { _ = closeState() }()

	if _, enrolled, err := state.Load(ctx); err != nil {
		return fmt.Errorf("read the enrollment: %w", err)
	} else if enrolled {
		s.logger().Info(alreadyEnrolledMessage)
		// A key stored in the config file must not stay on disk, also when
		// the process died between the commit and the cleanup.
		forgetStoredInviteKey(s)
		return nil
	}

	in, err := config.ResolveWorkerInviteKeyContext(ctx, s.Resolver, d.stdin)
	if err != nil {
		return err
	}
	if !in.Set {
		return nil
	}
	invite, err := identity.ParseInviteKey(in.Value)
	if err != nil {
		return errors.New("the invitation key is malformed: expected mnk_<hub fingerprint>.<secret>")
	}
	hub, err := config.ResolveWorkerHub(s.Resolver)
	if err != nil {
		return err
	}

	dialer := d.dialer
	if dialer == nil {
		dialer = NewInviteDialer()
	}
	enroller, err := NewInviteEnroller(InviteEnrollerConfig{
		State: state, Dialer: dialer, HubURL: hub.Server, Name: s.Name,
		Attempts: d.attempts, RetryWait: d.wait, Sleep: d.sleep, Logger: s.logger(),
	})
	if err != nil {
		return err
	}
	res, err := enroller.Enroll(ctx, invite)
	if err != nil {
		return err
	}
	forgetStoredInviteKey(s)
	if res.AlreadyEnrolled {
		return nil
	}
	if s.Out != nil {
		_, _ = fmt.Fprintf(s.Out, "enrolled as %s, owner %s\n", res.Enrollment.Name, res.Enrollment.Owner)
	}
	return nil
}

// forgetStoredInviteKey removes invite_key from the config file once the
// Worker is enrolled: enrollment is permanent and the key must not stay on
// disk. It runs under the config lock of the `worker config` commands and
// removes the key from a fresh copy of the file, so that a setting changed
// while the enrollment waited for the Hub is not overwritten. A failure is
// logged, never fatal, because the Worker is enrolled; the next start tries
// again.
func forgetStoredInviteKey(s Settings) {
	f, ok := s.Resolver.File.(storedConfig)
	if !ok {
		return
	}
	if _, has, _ := f.Lookup(config.KeyInviteKey); !has && !diskHasInviteKey(f.Path()) {
		return
	}
	fail := func(err error) {
		s.logger().Warn("cannot remove the invitation key from the config file; remove invite_key from it by hand", "error", err.Error())
	}
	release, err := lockConfig(filepath.Join(filepath.Dir(f.Path()), configLockName))
	if err != nil {
		fail(err)
		return
	}
	defer release()
	fresh, err := config.LoadFile(f.Path())
	if err != nil {
		fail(err)
		return
	}
	if _, has, _ := fresh.Lookup(config.KeyInviteKey); has {
		fresh.Delete(config.KeyInviteKey)
		if err := fresh.Save(); err != nil {
			fail(err)
			return
		}
	}
	f.Delete(config.KeyInviteKey)
}

// diskHasInviteKey reports whether the config file on disk holds invite_key,
// for the case that it was set after this process loaded the file.
func diskHasInviteKey(path string) bool {
	fresh, err := config.LoadFile(path)
	if err != nil {
		return false
	}
	_, has, _ := fresh.Lookup(config.KeyInviteKey)
	return has
}
