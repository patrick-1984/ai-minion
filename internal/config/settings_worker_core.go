package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// KeyCallAPIKeyFile names the file of the Worker call key (F-01).
	KeyCallAPIKeyFile = "call_api_key_file"
	// KeyPipeAcceptFrom is the receiver policy for messages from other Workers.
	KeyPipeAcceptFrom = "pipe_accept_from"
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
// are --tmux-socket, --name, --share-existing, --preview-only, --share-lock,
// --call-api-key-path (call_api_key_file) and --pipe-accept-from.
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
	r.Declare(Setting{
		Key:   KeyCallAPIKeyFile,
		Scope: ScopeWorker,
		// The registry reserves names ending in _FILE for the file form of
		// secrets; the key lives in the file, so this setting is a path.
		Env: "MINION_CALL_API_KEY_PATH",
		Help: "File with the Worker-bound API key (mn_...) that this Worker's local commands and local MCP use for " +
			"calls to other Workers through the Hub. Mode 0600, owned by the Worker's user; read again on every call. " +
			"Unset: this Worker can message only itself",
	})
	r.Declare(Setting{
		Key:   KeyPipeAcceptFrom,
		Scope: ScopeWorker,
		Help: "Which other Workers may send messages (pipe_send, send_text, send_keys, answer, set_task) to sessions of " +
			"this Worker: none (default), owner-workers (Workers of the same owner), or a comma-separated list of " +
			"Worker ids or fingerprints. Messages from the Worker itself are always accepted. Local setting only; " +
			"the Hub cannot change it",
		Default: PipeAcceptNone,
	})
}

// Values of pipe_accept_from besides a list of Worker ids and fingerprints.
const (
	PipeAcceptNone         = "none"
	PipeAcceptOwnerWorkers = "owner-workers"
)

// MaxPipeAcceptEntries bounds the list form of pipe_accept_from.
const MaxPipeAcceptEntries = 256

// PipeAcceptFrom is the resolved pipe_accept_from. The zero value is none.
type PipeAcceptFrom struct {
	// OwnerWorkers accepts every Worker whose owner is the owner of this Worker.
	OwnerWorkers bool
	// Allow lists the accepted Workers: ids as written, fingerprints in
	// canonical lowercase. Empty with OwnerWorkers false is none.
	Allow []string
}

// String returns the setting form: none, owner-workers or the list.
func (a PipeAcceptFrom) String() string {
	switch {
	case a.OwnerWorkers:
		return PipeAcceptOwnerWorkers
	case len(a.Allow) == 0:
		return PipeAcceptNone
	}
	return strings.Join(a.Allow, ",")
}

// ParsePipeAcceptFrom parses the value of pipe_accept_from. The empty string
// is none. The error never repeats the value.
func ParsePipeAcceptFrom(v string) (PipeAcceptFrom, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", PipeAcceptNone:
		return PipeAcceptFrom{}, nil
	case PipeAcceptOwnerWorkers:
		return PipeAcceptFrom{OwnerWorkers: true}, nil
	}
	var out PipeAcceptFrom
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
			return PipeAcceptFrom{}, fmt.Errorf("the list has an empty entry")
		case strings.EqualFold(e, PipeAcceptNone) || strings.EqualFold(e, PipeAcceptOwnerWorkers):
			return PipeAcceptFrom{}, fmt.Errorf("%s cannot be combined with other entries", PipeAcceptNone+" and "+PipeAcceptOwnerWorkers)
		case len(e) == 52:
			e = strings.ToLower(e)
			if !validFingerprintEntry(e) {
				return PipeAcceptFrom{}, fmt.Errorf("an entry is neither a Worker id nor a fingerprint")
			}
		default:
			if !validWorkerIDEntry(e) {
				return PipeAcceptFrom{}, fmt.Errorf("an entry is neither a Worker id nor a fingerprint")
			}
		}
		if !slices.Contains(out.Allow, e) {
			out.Allow = append(out.Allow, e)
		}
		if len(out.Allow) > MaxPipeAcceptEntries {
			return PipeAcceptFrom{}, fmt.Errorf("the list has more than %d entries", MaxPipeAcceptEntries)
		}
	}
	return out, nil
}

// validFingerprintEntry is the syntactic check of a lowercase fingerprint (AD-5):
// 52 characters of the RFC 4648 base32 alphabet, the last one canonical (the
// 4 unused bits of 256 bits are zero, so it is "a" or "q"). The config package
// may not import internal/identity (AD-1), so the check is repeated here; the
// consumer compares the entry with real fingerprints, so a lookalike never
// matches anything.
func validFingerprintEntry(e string) bool {
	if len(e) != 52 {
		return false
	}
	for _, r := range e {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return e[51] == 'a' || e[51] == 'q'
}

// validWorkerIDEntry accepts the shape of a Worker id, a UUID in the canonical
// 8-4-4-4-12 hexadecimal form, and nothing else.
func validWorkerIDEntry(e string) bool {
	if len(e) != 36 {
		return false
	}
	for i, r := range e {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F'):
			return false
		}
	}
	return true
}

// defaultWorkerName is the short host name (the part before the first dot),
// cut to MaxNameLength characters, or "minion" when the OS reports none. A
// long host name (CI runners, some cloud hosts) must not stop a Worker that
// was given no name; an explicit name is still validated strictly.
func defaultWorkerName() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "minion", nil
	}
	return shortWorkerName(h), nil
}

// shortWorkerName derives the default Worker name from a host name.
func shortWorkerName(host string) string {
	h := strings.TrimSpace(host)
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	if r := []rune(h); len(r) > MaxNameLength {
		h = string(r[:MaxNameLength])
	}
	h = strings.TrimSpace(h)
	if h == "" {
		return "minion"
	}
	return h
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
	// CallAPIKeyFile is the file of the Worker call key; empty when unset. It is
	// the path only: the key is read again on every call.
	CallAPIKeyFile string
	// PipeAcceptFrom is the receiver policy for messages from other Workers.
	PipeAcceptFrom PipeAcceptFrom
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
	ck, err := get(KeyCallAPIKeyFile)
	if err != nil {
		return wc, err
	}
	if p := strings.TrimSpace(ck.Value); p != "" {
		if wc.CallAPIKeyFile, err = filepath.Abs(p); err != nil {
			return wc, fmt.Errorf("setting %q: %w", KeyCallAPIKeyFile, err)
		}
	}
	acc, err := get(KeyPipeAcceptFrom)
	if err != nil {
		return wc, err
	}
	if wc.PipeAcceptFrom, err = ParsePipeAcceptFrom(acc.Value); err != nil {
		return wc, fmt.Errorf("setting %q (%s): %w", KeyPipeAcceptFrom, acc.Source, err)
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
