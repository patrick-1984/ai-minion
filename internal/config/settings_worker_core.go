package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Names of the core Worker settings (Story 2.7), for callers that look them
// up by key.
const (
	KeyTmuxSocket    = "tmux_socket"
	KeyName          = "name"
	KeyShareExisting = "share_existing"
	KeyPreviewOnly   = "preview_only"
	KeyShareLock     = "share_lock"
)

// Values of share_existing. They match the share modes of the Worker state
// database; the empty value means "not set".
const (
	ShareExistingAll   = "all"
	ShareExistingNone  = "none"
	ShareExistingLater = "later"
)

// MaxNameLength bounds the Worker name, in characters.
const MaxNameLength = 64

func init() { declareWorkerCore(Default) }

// declareWorkerCore declares the settings every Worker run needs. The flags
// are --tmux-socket, --name, --share-existing, --preview-only and --share-lock.
func declareWorkerCore(r *Registry) {
	r.Declare(Setting{
		Key:   KeyTmuxSocket,
		Scope: ScopeWorker,
		Help: "Path of the tmux server socket the Worker drives; the default is the user's default socket " +
			"(<TMUX_TMPDIR or /tmp>/tmux-<uid>/default)",
		DefaultFunc: func() (string, error) { return SystemPathEnv().TmuxSocket() },
	})
	r.Declare(Setting{
		Key:         KeyName,
		Scope:       ScopeWorker,
		Help:        "Display name of this Worker; the default is the host name",
		DefaultFunc: defaultWorkerName,
	})
	r.Declare(Setting{
		Key:   KeyShareExisting,
		Scope: ScopeWorker,
		Help: "Which tmux sessions are shared at start: all, none or later (nothing shared until decided). " +
			"Unset keeps the saved choice",
		Choices: []string{ShareExistingAll, ShareExistingNone, ShareExistingLater},
	})
	r.Declare(Setting{
		Key:     KeyPreviewOnly,
		Scope:   ScopeWorker,
		Help:    "Refuse every write to a session: sessions can be listed and read only (true or false)",
		Default: "false",
		Choices: []string{"true", "false"},
	})
	r.Declare(Setting{
		Key:   KeyShareLock,
		Scope: ScopeWorker,
		Help: "Lock the share selection locally: the Hub can no longer change which sessions are shared " +
			"(share_set is refused); you still change it here (true or false)",
		Default: "false",
		Choices: []string{"true", "false"},
	})
}

// defaultWorkerName is the host name, or "minion" when the OS reports none.
func defaultWorkerName() (string, error) {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "minion", nil
	}
	return h, nil
}

// TmuxSocket returns the socket a tmux client without -S and -L uses when it
// is not asked otherwise: <TMUX_TMPDIR or /tmp>/tmux-<uid>/default, as an
// absolute path. tmux does not exist on Windows (ConPTY hosts the sessions
// there), so the result is empty.
func (p PathEnv) TmuxSocket() (string, error) {
	if p.GOOS == "windows" {
		return "", nil
	}
	dir := p.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("cannot make TMUX_TMPDIR %q absolute: %w", dir, err)
	}
	return filepath.Join(abs, "tmux-"+strconv.Itoa(p.UID), "default"), nil
}

// WorkerCore is the resolved core configuration of a Worker run. The tmux
// socket is resolved here once and handed to every module (AD-13).
type WorkerCore struct {
	// StateDir is the absolute Worker state directory.
	StateDir string
	// TmuxSocket is the absolute tmux socket path. It is empty on Windows,
	// where sessions are hosted by ConPTY.
	TmuxSocket string
	// Name is the Worker's display name.
	Name string
	// ShareExisting is "all", "none", "later", or "" when no source set it:
	// the saved share selection then stays as it is.
	ShareExisting string
	// PreviewOnly refuses every write to a session.
	PreviewOnly bool
	// ShareLock refuses a change of the share selection from the Hub (share_set).
	ShareLock bool
}

// ResolveWorkerCore resolves the core Worker settings with res (a Worker
// resolver) and validates them. A relative path is made absolute against the
// current directory, once, so that nothing later depends on where the process
// changes to.
func ResolveWorkerCore(res *Resolver) (WorkerCore, error) {
	var wc WorkerCore
	get := func(key string) (Resolved, error) { return res.Resolve(key) }

	state, err := get(KeyStateDir)
	if err != nil {
		return wc, err
	}
	if state.Value == "" {
		return wc, fmt.Errorf("setting %q is empty; give a state directory", KeyStateDir)
	}
	if wc.StateDir, err = filepath.Abs(state.Value); err != nil {
		return wc, fmt.Errorf("setting %q: %w", KeyStateDir, err)
	}

	sock, err := get(KeyTmuxSocket)
	if err != nil {
		return wc, err
	}
	if sock.Value == "" && SystemPathEnv().GOOS != "windows" {
		return wc, fmt.Errorf("setting %q is empty; give the path of a tmux socket or leave it unset for the default", KeyTmuxSocket)
	}
	if sock.Value != "" {
		if wc.TmuxSocket, err = filepath.Abs(sock.Value); err != nil {
			return wc, fmt.Errorf("setting %q: %w", KeyTmuxSocket, err)
		}
	}

	name, err := get(KeyName)
	if err != nil {
		return wc, err
	}
	if wc.Name, err = validateWorkerName(name.Value); err != nil {
		return wc, fmt.Errorf("setting %q (%s): %w", KeyName, name.Source, err)
	}

	share, err := get(KeyShareExisting)
	if err != nil {
		return wc, err
	}
	wc.ShareExisting = share.Value

	prev, err := get(KeyPreviewOnly)
	if err != nil {
		return wc, err
	}
	if wc.PreviewOnly, err = strconv.ParseBool(prev.Value); err != nil {
		return wc, fmt.Errorf("setting %q (%s): invalid value %q, expected true or false", KeyPreviewOnly, prev.Source, prev.Value)
	}
	lock, err := get(KeyShareLock)
	if err != nil {
		return wc, err
	}
	if wc.ShareLock, err = strconv.ParseBool(lock.Value); err != nil {
		return wc, fmt.Errorf("setting %q (%s): invalid value %q, expected true or false", KeyShareLock, lock.Source, lock.Value)
	}
	return wc, nil
}

// validateWorkerName trims the name and checks it is a short single line of
// printable characters.
func validateWorkerName(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("the name is empty")
	}
	if utf8.RuneCountInString(v) > MaxNameLength {
		return "", fmt.Errorf("the name is longer than %d characters", MaxNameLength)
	}
	for _, r := range v {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return "", fmt.Errorf("the name holds a control or invalid character")
		}
	}
	return v, nil
}
