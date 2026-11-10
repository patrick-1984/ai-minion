package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// paneForm is the "window.pane" part of a target.
var paneForm = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// paneID is a tmux pane id such as %3, the only form of $TMUX_PANE that is
// passed on to tmux.
var paneID = regexp.MustCompile(`^%[0-9]+$`)

// ParseTargetFlag parses the value of --target: "session", "session:W.P",
// "worker-id:session" or "worker-id:session:W.P". Session names never contain
// ":" or "." (tmux rewrites them), so the forms are unambiguous. The Worker is
// named by its id (a UUIDv7); on the local socket the Worker part is left out.
func ParseTargetFlag(s string) (ops.Target, error) {
	parts := strings.Split(s, ":")
	var t ops.Target
	switch {
	case len(parts) == 1:
		t.SessionName = parts[0]
	case len(parts) == 2 && paneForm.MatchString(parts[1]):
		t.SessionName, t.Pane = parts[0], parts[1]
	case len(parts) == 2:
		t.WorkerID, t.SessionName = parts[0], parts[1]
	case len(parts) == 3:
		t.WorkerID, t.SessionName, t.Pane = parts[0], parts[1], parts[2]
	default:
		return ops.Target{}, errors.New("expected [worker-id:]session[:window.pane]")
	}
	if t.WorkerID != "" && !ops.ValidUUIDv7(t.WorkerID) {
		return ops.Target{}, errors.New("the Worker must be given by its id (a UUIDv7); for a session of the local Worker give just the session name")
	}
	if err := t.Validate(); err != nil {
		return ops.Target{}, err
	}
	return t, nil
}

// SessionResolver finds the tmux session of the pane this process runs in. It
// returns "" without an error when the process is not inside a tmux pane.
type SessionResolver func(ctx context.Context) (string, error)

// sessionLister is the part of the tmux adapter the default resolver and the
// server check use.
type sessionLister interface {
	ListSessions(ctx context.Context) ([]term.Session, error)
	SetOption(ctx context.Context, session, key, value string) error
}

// ServerConfirmer proves that the tmux server of the current pane is the one
// the Worker serves, before an implicit target (the current session) is
// accepted. It returns an error when that cannot be established.
type ServerConfirmer func(ctx context.Context, session string, worker client.Backend) error

// newSessionLister opens the tmux adapter on an absolute socket path. Only the
// adapter runs tmux (AD-1), and it always passes -S <socket> (AD-13).
func newSessionLister(socket string) (sessionLister, error) {
	return tmux.New(socket)
}

// tmuxSocketFromEnv returns the socket path of $TMUX, which tmux sets to
// "<socket>,<server pid>,<session index>": everything before the last two
// comma-separated fields (a socket path may itself contain a comma).
func tmuxSocketFromEnv(v string) (string, error) {
	parts := strings.Split(v, ",")
	if len(parts) < 3 {
		return "", fmt.Errorf("TMUX=%q is not of the form socket,pid,index", v)
	}
	return strings.Join(parts[:len(parts)-2], ","), nil
}

// shellPane returns the socket of the tmux server of the current pane and the
// pane id, from $TMUX and $TMUX_PANE. ok is false when the process is not
// inside a tmux pane.
func shellPane(getenv func(string) string) (socket, pane string, ok bool, err error) {
	env, pane := getenv("TMUX"), getenv("TMUX_PANE")
	if env == "" || pane == "" {
		return "", "", false, nil
	}
	if !paneID.MatchString(pane) {
		return "", "", false, fmt.Errorf("TMUX_PANE=%q is not a tmux pane id", pane)
	}
	socket, err = tmuxSocketFromEnv(env)
	if err != nil {
		return "", "", false, err
	}
	return socket, pane, true, nil
}

// findPaneSession returns the session that owns pane on the tmux server host
// talks to.
func findPaneSession(ctx context.Context, host sessionLister, socket, pane string) (term.Session, error) {
	sessions, err := host.ListSessions(ctx)
	if err != nil {
		return term.Session{}, fmt.Errorf("find the tmux session of pane %s: %w", pane, err)
	}
	for _, s := range sessions {
		for _, p := range s.Panes {
			if p.Ref.ID == pane && s.Name != "" {
				return s, nil
			}
		}
	}
	return term.Session{}, fmt.Errorf("pane %s is in no session of the tmux server %s", pane, socket)
}

// resolveTmuxSession is the default [SessionResolver]. It reads $TMUX (the
// socket of the tmux server the pane belongs to) and $TMUX_PANE, lists the
// sessions of that server through the tmux adapter and returns the name of the
// session that owns the pane. An empty $TMUX or $TMUX_PANE means the process
// is not inside tmux. getenv and open are the seams of tests.
//
// The name alone does not say which tmux server it belongs to: the caller must
// run a [ServerConfirmer] (see confirmTmuxServer) before it uses the name.
func resolveTmuxSession(getenv func(string) string, open func(socket string) (sessionLister, error)) SessionResolver {
	return func(ctx context.Context) (string, error) {
		socket, pane, ok, err := shellPane(getenv)
		if err != nil || !ok {
			return "", err
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		host, err := open(socket)
		if err != nil {
			return "", fmt.Errorf("open the tmux server of pane %s: %w", pane, err)
		}
		s, err := findPaneSession(ctx, host, socket, pane)
		if err != nil {
			return "", err
		}
		return s.Name, nil
	}
}

// probeKey is the dedicated session option the server check writes its nonce
// to. The Worker echoes it as the "probe" of sessions_list, so it is the one
// channel that proves two parties talk to the same tmux server. It is separate
// from the task option on purpose: the check never reads, replaces or restores
// user text.
const probeKey = tmux.OptionProbe

// probeNoncePrefix starts every nonce, so that a leftover value is
// recognisable when someone looks at the option.
const probeNoncePrefix = "minion-probe-"

// probeLocker takes the lock that serialises the probes of one session. key
// identifies the shell's tmux server and the session; the result releases it.
type probeLocker func(ctx context.Context, key string) (release func(), err error)

// defaultProbeLocker locks a file in the runtime directory of this user, named
// after a hash of key, so that overlapping mn commands in one session probe
// one at a time (also across processes).
func defaultProbeLocker(ctx context.Context, key string) (func(), error) {
	dir, err := config.SystemPathEnv().RuntimeDir()
	if err != nil {
		return nil, fmt.Errorf("find the runtime directory: %w", err)
	}
	sum := sha256.Sum256([]byte(key))
	return lockFile(ctx, filepath.Join(dir, "probe-"+hex.EncodeToString(sum[:8])+".lock"))
}

// errServerNotConfirmed is the refusal when the Worker did not echo the nonce.
// It also covers a Worker that predates the probe field: such a Worker never
// echoes it, so the check fails closed, and the message names the way out.
func errServerNotConfirmed(socket string) error {
	return fmt.Errorf("the tmux server of this pane (%s) is not the one the Worker serves, or the Worker is too old to prove it (update the Worker); pass --target <session> to name the session explicitly", socket)
}

// confirmTmuxServer is the default [ServerConfirmer]. The session name from
// $TMUX_PANE is only meaningful on the tmux server of the shell, and the Worker
// serves its own server (its --tmux-socket); both may hold a session with the
// same name. Before the name is accepted as the implicit target, a random
// nonce is stored in the dedicated probe option of the current session on the
// shell's server and the Worker is asked to list its sessions: only a Worker
// on the same server reports the nonce as the probe of that session. Anything
// that does not prove the identity (no Worker reachable, session not shared,
// nonce not echoed) is an error.
//
// The probe never touches the task text of any session: it writes only the
// probe option, which holds no user data, so there is nothing to read back or
// restore and a concurrent set_task cannot be overwritten. Afterwards the
// nonce is cleared when the option still holds it. Probes of one session run
// one at a time (lock), the only other writers of that option.
func confirmTmuxServer(getenv func(string) string, open func(socket string) (sessionLister, error), nonce func() (string, error), lock probeLocker) ServerConfirmer {
	return func(ctx context.Context, session string, worker client.Backend) (err error) {
		socket, pane, ok, err := shellPane(getenv)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("not inside a tmux pane")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		host, err := open(socket)
		if err != nil {
			return fmt.Errorf("open the tmux server of pane %s: %w", pane, err)
		}
		release, err := lock(ctx, socket+"\x00"+session)
		if err != nil {
			return err
		}
		defer release()
		cur, err := findPaneSession(ctx, host, socket, pane)
		if err != nil {
			return err
		}
		if cur.Name != session {
			return fmt.Errorf("pane %s is in session %q, not %q", pane, cur.Name, session)
		}
		// Cheap refusal first: a Worker that does not list the session cannot
		// serve it, and nothing is written.
		listed, err := workerSessions(ctx, worker)
		if err != nil {
			return err
		}
		if _, found := sessionNamed(listed, session); !found {
			return fmt.Errorf("the Worker does not share a session named %q", session)
		}
		n, err := nonce()
		if err != nil {
			return fmt.Errorf("create the probe value: %w", err)
		}
		// The cleanup is registered before the nonce is written: a SetOption
		// that applied the value but returned an error (a timeout) would
		// otherwise leave the nonce behind. Only the probe option is touched,
		// and only while it still holds this probe's nonce.
		defer func() {
			// Clean up even when ctx ended.
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer rcancel()
			now, lerr := findPaneSession(rctx, host, socket, pane)
			switch {
			case lerr != nil:
				if err == nil {
					err = fmt.Errorf("clear the probe of session %q: %w", session, lerr)
				}
			case now.Name != session || now.Probe != n:
				// Not this probe's marker any more: leave the option alone.
			default:
				if rerr := host.SetOption(rctx, session, probeKey, ""); rerr != nil && err == nil {
					err = fmt.Errorf("clear the probe of session %q: %w", session, rerr)
				}
			}
		}()
		if err := host.SetOption(ctx, session, probeKey, n); err != nil {
			return fmt.Errorf("mark the current session: %w", err)
		}
		listed, err = workerSessions(ctx, worker)
		if err != nil {
			return err
		}
		if s, found := sessionNamed(listed, session); !found || s.Probe != n {
			return errServerNotConfirmed(socket)
		}
		return nil
	}
}

func workerSessions(ctx context.Context, worker client.Backend) ([]ops.SessionInfo, error) {
	raw, err := worker.Call(ctx, ops.OpSessionsList, ops.SessionsListArgs{})
	if err != nil {
		return nil, fmt.Errorf("ask the Worker which tmux server it serves: %w", err)
	}
	var res ops.SessionsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("the Worker's session list is not valid: %w", err)
	}
	return res.Sessions, nil
}

func sessionNamed(list []ops.SessionInfo, name string) (ops.SessionInfo, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return ops.SessionInfo{}, false
}

// newProbeNonce returns a fresh random probe value.
func newProbeNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return probeNoncePrefix + hex.EncodeToString(b[:]), nil
}

// targetMember resolves the target of a call: the --target value when given,
// else the current tmux session. The result is the JSON member of "target";
// implicit is the session name when it came from the current tmux pane (it
// still has to be confirmed with a [ServerConfirmer]) and "" otherwise.
func targetMember(ctx context.Context, value string, given bool, resolve SessionResolver) (raw json.RawMessage, implicit string, err error) {
	var t ops.Target
	if given {
		if t, err = ParseTargetFlag(value); err != nil {
			return nil, "", usagef("--target: %v", err)
		}
	} else {
		name, err := resolve(ctx)
		if err != nil {
			return nil, "", usagef("no --target given and the current session cannot be found: %v", err)
		}
		if name == "" {
			return nil, "", usagef("no target: pass --target <session> (or run inside a tmux pane, where the current session is the default)")
		}
		t = ops.Target{SessionName: name}
		implicit = name
	}
	raw, err = json.Marshal(t)
	return raw, implicit, err
}
