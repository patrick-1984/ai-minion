package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Step names one of the three steps of the wizard, in order (UX-DR1).
type Step int

// The steps of the wizard.
const (
	// StepShare asks which tmux sessions the Hub may see (R75).
	StepShare Step = iota + 1
	// StepHub asks for the Hub address and checks it (R76).
	StepHub
	// StepPair shows the pairing code and waits for the Hub (R77).
	StepPair
)

// Errors returned by the engine. Test with [errors.Is].
var (
	// ErrCancelled means the user ended the wizard before the settings were
	// saved. Nothing was changed.
	ErrCancelled = errors.New("setup was cancelled; nothing was changed")
	// ErrOutOfOrder means a step was used before the step it needs.
	ErrOutOfOrder = errors.New("setup steps were used out of order")
	// ErrInvalidChoice means the answer to a question is not acceptable. The
	// message says why in plain English.
	ErrInvalidChoice = errors.New("that is not a valid choice")
	// ErrHubChange means the Worker is already paired with another Hub than
	// the address chosen. Nothing was changed: pairing is permanent for its
	// Hub, and only the explicit re-pairing (`minion worker join <address>
	// --new-code`) or `minion worker reset` replaces it.
	ErrHubChange = errors.New("this Worker is paired with a different Hub")
)

// ValidateHubAddress validates raw as the address of a Hub for the wizard: the
// rules of [config.ValidateServerURL] plus no query and no fragment. The
// Worker's connection ignores both, so a pasted address that carries them
// (often a token) is refused early instead of being shown or saved. The error
// never repeats raw. The empty string is valid and means "no Hub".
func ValidateHubAddress(raw string) (string, error) {
	addr, err := config.ValidateServerURL(raw)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(addr, "?#") {
		return "", fmt.Errorf("%w: it must not contain a query (?...) or a fragment (#...); "+
			"the Worker does not use them, so leave them out", config.ErrInvalidServer)
	}
	return addr, nil
}

// SessionLister lists the tmux sessions the Worker can see. term.SessionHost
// implements it.
type SessionLister interface {
	ListSessions(ctx context.Context) ([]term.Session, error)
}

// Saved is what the Worker has saved now.
type Saved struct {
	// Share is the saved share selection; Configured is false on a first run.
	Share store.ShareSelection
	// Server is the saved Hub address, empty for Solo.
	Server string
}

// Settings is what the wizard saves, in one call.
type Settings struct {
	// Share is the share choice. Mode select carries the exact session names.
	Share store.ShareSelection
	// Server is the Hub address; empty means Solo and removes a saved address.
	Server string
}

// Persister saves the wizard's choices. The implementation decides how and
// where (the config file and the Worker state database); it must either save
// everything or report an error and leave the previous settings in place.
type Persister interface {
	// Current returns what is saved now, to offer it as the default.
	Current(ctx context.Context) (Saved, error)
	// Save writes the settings.
	Save(ctx context.Context, s Settings) error
}

// HubChecker checks a Hub address. [Checker] implements it.
type HubChecker interface {
	Check(ctx context.Context, hubURL string) Report
}

// Paired is the result of a pairing that completed.
type Paired struct {
	// Owner is the user the Worker is paired to.
	Owner string
	// Hub is the address of the Hub of the pairing, empty when it is not
	// recorded (an enrollment by invitation).
	Hub string
}

// Pairing is the pairing port: the existing pairing flow of the Worker. The
// wizard adds no pairing logic and no cryptography of its own.
type Pairing interface {
	// Paired reports whether the Worker is already paired, and to whom.
	Paired(ctx context.Context) (Paired, bool, error)
	// Pair shows a pairing code on out (or the reason none is available yet)
	// and waits for the Hub to confirm it. It returns when the pairing
	// completes or ctx ends. out receives the code and the "paired to" line.
	Pair(ctx context.Context, hubURL string, out io.Writer) (Paired, error)
}

// Provided holds the values that flags or environment variables already
// decide. A provided value is never asked for (R85): the wizard uses it,
// reports what it did, and saves it with the rest.
type Provided struct {
	// Share is the share mode that is already decided: all, none or later.
	// Empty means the question is asked.
	Share store.ShareMode
	// Server and HasServer give the Hub address that is already decided.
	// HasServer with an empty Server is an explicit Solo.
	Server    string
	HasServer bool
}

// Config is the wiring of a [Wizard].
type Config struct {
	// Sessions lists the sessions to choose from; nil means they cannot be
	// listed (for example on Windows).
	Sessions SessionLister
	// Persist saves the choices. Required.
	Persist Persister
	// Check checks the Hub address. Required for the Hub step.
	Check HubChecker
	// Pairing is the pairing port; nil means pairing is not available here.
	Pairing Pairing
	// Provided are the values that flags or environment already decide.
	Provided Provided
}

// Wizard is the first-run step engine (AD-21). It holds the answers and
// enforces the order of the steps, and it does no input or output of its own,
// so that the terminal wizard and the local web wizard (Story 4.14) drive the
// same engine. It is not safe for concurrent use.
type Wizard struct {
	cfg Config

	saved     Saved
	loaded    bool
	sessions  []string
	listErr   error
	listed    bool
	share     *store.ShareSelection
	hubState  hubState
	hubURL    string // the accepted address; empty with hubState==hubSolo
	candidate string // the address of the last check
	report    *Report
	persisted bool
}

type hubState int

const (
	hubUnset hubState = iota
	hubSolo
	hubAccepted
)

// New returns a wizard.
func New(cfg Config) *Wizard { return &Wizard{cfg: cfg} }

// ShareView is what the first step shows.
type ShareView struct {
	// Sessions are the names of the tmux sessions, in the order the host
	// reports them. Names only: the wizard never reads session content.
	Sessions []string
	// Listed is false when the sessions could not be listed; ListError then
	// says why and "choose now" is not possible.
	Listed    bool
	ListError string
	// Default is the choice to offer first: the saved one, else all.
	Default store.ShareSelection
	// Decided is the provided mode when flags or environment already decide
	// the question; empty when it has to be asked.
	Decided store.ShareMode
}

func (w *Wizard) load(ctx context.Context) error {
	if w.loaded {
		return nil
	}
	if w.cfg.Persist == nil {
		return errors.New("setup: no way to save the settings")
	}
	saved, err := w.cfg.Persist.Current(ctx)
	if err != nil {
		return fmt.Errorf("read the saved settings: %w", err)
	}
	w.saved, w.loaded = saved, true
	return nil
}

// ShareStep returns what step 1 shows.
func (w *Wizard) ShareStep(ctx context.Context) (ShareView, error) {
	if err := w.load(ctx); err != nil {
		return ShareView{}, err
	}
	if !w.listed {
		w.listed = true
		if w.cfg.Sessions == nil {
			w.listErr = errors.New("this computer has no tmux that the Worker can list here")
		} else {
			list, err := w.cfg.Sessions.ListSessions(ctx)
			if err != nil {
				w.listErr = err
			}
			for _, s := range list {
				w.sessions = append(w.sessions, s.Name)
			}
		}
	}
	v := ShareView{Sessions: append([]string(nil), w.sessions...), Listed: w.listErr == nil, Decided: w.cfg.Provided.Share}
	if w.listErr != nil {
		v.ListError = w.listErr.Error()
	}
	v.Default = store.ShareSelection{Mode: store.ShareAll, Sessions: []string{}}
	if w.saved.Share.Configured {
		v.Default = store.ShareSelection{Mode: w.saved.Share.Mode, Sessions: append([]string{}, w.saved.Share.Sessions...)}
		if v.Default.Mode == store.ShareSelect {
			v.Default.Sessions = intersect(v.Default.Sessions, w.sessions)
		}
	}
	return v, nil
}

// ChooseShare records the answer to step 1. A choice of select must name at
// least one session that exists now; the names are kept exactly as the host
// reports them. Sessions are never renamed, stopped or changed.
func (w *Wizard) ChooseShare(ctx context.Context, c store.ShareSelection) error {
	if _, err := w.ShareStep(ctx); err != nil {
		return err
	}
	switch c.Mode {
	case store.ShareAll, store.ShareNone, store.ShareLater:
		w.share = &store.ShareSelection{Mode: c.Mode, Sessions: append([]string{}, w.saved.Share.Sessions...)}
		return nil
	case store.ShareSelect:
		if w.listErr != nil {
			return fmt.Errorf("%w: the sessions cannot be listed, so there is nothing to choose from", ErrInvalidChoice)
		}
		if len(c.Sessions) == 0 {
			return fmt.Errorf("%w: tick at least one session, or choose another option", ErrInvalidChoice)
		}
		have := map[string]bool{}
		for _, n := range w.sessions {
			have[n] = true
		}
		seen := map[string]bool{}
		var names []string
		for _, n := range c.Sessions {
			if !have[n] {
				return fmt.Errorf("%w: a chosen session no longer exists", ErrInvalidChoice)
			}
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		w.share = &store.ShareSelection{Mode: store.ShareSelect, Sessions: names}
		return nil
	}
	return fmt.Errorf("%w: use all, choose, later or none", ErrInvalidChoice)
}

// ApplyProvided records the values that flags or environment decide, so that
// the steps they cover ask nothing. It fails when a provided share mode is not
// valid.
func (w *Wizard) ApplyProvided(ctx context.Context) error {
	if m := w.cfg.Provided.Share; m != "" {
		if err := w.ChooseShare(ctx, store.ShareSelection{Mode: m}); err != nil {
			return err
		}
	}
	return nil
}

// HubView is what step 2 shows.
type HubView struct {
	// Default is the saved Hub address to offer, empty for none.
	Default string
	// Decided is true when flags or environment already decide the address;
	// Server then holds it (empty for an explicit Solo).
	Decided bool
	Server  string
}

// HubStep returns what step 2 shows. It needs the share choice first.
func (w *Wizard) HubStep(ctx context.Context) (HubView, error) {
	if w.share == nil {
		return HubView{}, ErrOutOfOrder
	}
	if err := w.load(ctx); err != nil {
		return HubView{}, err
	}
	return HubView{Default: w.saved.Server, Decided: w.cfg.Provided.HasServer, Server: w.cfg.Provided.Server}, nil
}

// CheckHub validates raw as a Hub address and checks it: DNS, TCP, TLS and
// protocol. The report is for the user to read. The address is not accepted
// yet: call [Wizard.UseHub] to keep it (also when the check failed, "save
// anyway"), or call CheckHub again with a corrected one. An address that is
// not valid at all is [ErrInvalidChoice]; nothing was checked.
func (w *Wizard) CheckHub(ctx context.Context, raw string) (Report, error) {
	if w.share == nil {
		return Report{}, ErrOutOfOrder
	}
	addr, err := ValidateHubAddress(raw)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrInvalidChoice, err)
	}
	if addr == "" {
		return Report{}, fmt.Errorf("%w: type an address, or skip to run the Worker alone", ErrInvalidChoice)
	}
	if err := w.checkHubChange(ctx, addr); err != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrInvalidChoice, err)
	}
	if w.cfg.Check == nil {
		return Report{}, errors.New("setup: no way to check the Hub address")
	}
	rep := w.cfg.Check.Check(ctx, addr)
	w.candidate, w.report = addr, &rep
	return rep, nil
}

// UseHub accepts the address of the last [Wizard.CheckHub], whatever its
// result. The caller decides: a clean report is accepted at once, a failed one
// only when the user chose "save anyway".
func (w *Wizard) UseHub() error {
	if w.share == nil || w.report == nil {
		return ErrOutOfOrder
	}
	w.hubState, w.hubURL = hubAccepted, w.candidate
	return nil
}

// SkipHub chooses Solo: the Worker runs alone, without a Hub and without
// pairing.
func (w *Wizard) SkipHub() error {
	if w.share == nil {
		return ErrOutOfOrder
	}
	w.hubState, w.hubURL = hubSolo, ""
	return nil
}

// Summary is what will be saved.
type Summary struct {
	Share store.ShareSelection
	// Server is the Hub address, empty for Solo.
	Server string
}

// Summary returns the choices made so far.
func (w *Wizard) Summary() (Summary, error) {
	if w.share == nil || w.hubState == hubUnset {
		return Summary{}, ErrOutOfOrder
	}
	return Summary{Share: *w.share, Server: w.hubURL}, nil
}

// checkHubChange refuses addr when the Worker is paired with another Hub. The
// pin of the pairing belongs to that Hub: saving a different address would
// leave a Worker that cannot authenticate it, and would report the old pairing
// as done. The previous settings stay as they are until the user decides with
// `minion worker join <address> --new-code` (re-pairing) or `minion worker
// reset`. The Hub of the pairing is the recorded one, else the saved address.
func (w *Wizard) checkHubChange(ctx context.Context, addr string) error {
	if addr == "" || w.cfg.Pairing == nil {
		return nil
	}
	if err := w.load(ctx); err != nil {
		return err
	}
	p, ok, err := w.cfg.Pairing.Paired(ctx)
	if err != nil {
		return fmt.Errorf("read the pairing: %w", err)
	}
	if !ok {
		return nil
	}
	known := p.Hub
	if known == "" {
		known = w.saved.Server
	}
	if known == "" || sameHub(known, addr) {
		return nil
	}
	who := "this Worker"
	if p.Owner != "" {
		who += " (paired to " + DisplayName(p.Owner) + ")"
	}
	return hubChangeError{msg: fmt.Sprintf("%s is paired with the Hub at %s, not the one at %s. Nothing was changed. "+
		"To use the new Hub, pair again with `minion worker join <address> --new-code` (it revokes the old pairing), "+
		"or forget this Worker's Hub with `minion worker reset`", who, redact(known), redact(addr))}
}

// hubChangeError is [ErrHubChange] with the explanation for the user. The
// message holds only redacted addresses.
type hubChangeError struct{ msg string }

func (e hubChangeError) Error() string        { return e.msg }
func (e hubChangeError) Is(target error) bool { return target == ErrHubChange }

// sameHub reports whether two Hub addresses name the same Hub: the scheme
// family (TLS or plain), the host and the port, ignoring case, default ports
// and a trailing slash.
func sameHub(a, b string) bool {
	ka, oka := hubKey(a)
	kb, okb := hubKey(b)
	if !oka || !okb {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return ka == kb
}

func hubKey(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	secure := false
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		secure = true
	case "http", "ws":
	default:
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if secure {
			port = "443"
		}
	}
	return fmt.Sprintf("%t|%s|%s|%s", secure, strings.ToLower(u.Hostname()), port, strings.TrimRight(u.EscapedPath(), "/")), true
}

// Save saves the choices. Before it is called nothing has been written, so a
// wizard that ends earlier changes nothing. After it succeeds the settings are
// in force for the next start of the Worker. It refuses an address of another
// Hub than the one the Worker is paired with (see [ErrHubChange]).
func (w *Wizard) Save(ctx context.Context) error {
	sum, err := w.Summary()
	if err != nil {
		return err
	}
	if err := w.checkHubChange(ctx, sum.Server); err != nil {
		return err
	}
	sel := sum.Share
	sel.Configured = true
	if err := w.cfg.Persist.Save(ctx, Settings{Share: sel, Server: sum.Server}); err != nil {
		return err
	}
	w.persisted = true
	return nil
}

// PairOutcome is the result of step 3.
type PairOutcome struct {
	// Paired is true when the Worker is paired to Owner now.
	Paired bool
	// Owner is the user the Worker is paired to.
	Owner string
	// Already is true when the Worker was paired before the wizard asked.
	Already bool
	// Skipped says why no pairing ran: "solo" for a Worker without a Hub, and
	// "unavailable" when this wizard has no pairing.
	Skipped string
}

// Pair runs step 3 after [Wizard.Save]. For a Worker without a Hub it does
// nothing; for a Worker that is already paired it says so; otherwise it shows
// the pairing code on out and waits for the Hub to confirm it, until ctx ends.
// A pairing that ends with ctx is not an error: the settings are saved and the
// Worker can be paired later with `minion worker pair`.
func (w *Wizard) Pair(ctx context.Context, out io.Writer) (PairOutcome, error) {
	if !w.persisted {
		return PairOutcome{}, ErrOutOfOrder
	}
	if w.hubURL == "" {
		return PairOutcome{Skipped: "solo"}, nil
	}
	if w.cfg.Pairing == nil {
		return PairOutcome{Skipped: "unavailable"}, nil
	}
	if p, ok, err := w.cfg.Pairing.Paired(ctx); err != nil {
		return PairOutcome{}, fmt.Errorf("read the pairing: %w", err)
	} else if ok {
		return PairOutcome{Paired: true, Owner: p.Owner, Already: true}, nil
	}
	p, err := w.cfg.Pairing.Pair(ctx, w.hubURL, out)
	if err != nil {
		if ctx.Err() != nil {
			return PairOutcome{}, nil
		}
		return PairOutcome{}, err
	}
	return PairOutcome{Paired: true, Owner: p.Owner}, nil
}

func intersect(names, have []string) []string {
	in := map[string]bool{}
	for _, n := range have {
		in[n] = true
	}
	out := []string{}
	for _, n := range names {
		if in[n] {
			out = append(out, n)
		}
	}
	return out
}

// DescribeShare returns a plain-English line for a share selection.
func DescribeShare(sel store.ShareSelection) string {
	switch sel.Mode {
	case store.ShareAll:
		return "share all tmux sessions, also the ones created later"
	case store.ShareNone:
		return "share no tmux session"
	case store.ShareLater:
		return "decide later: nothing is shared until you choose"
	case store.ShareSelect:
		names := make([]string, len(sel.Sessions))
		for i, n := range sel.Sessions {
			names[i] = DisplayName(n)
		}
		sort.Strings(names)
		return "share only " + plural(len(names), "session") + ": " + strings.Join(names, ", ")
	}
	return string(sel.Mode)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// DisplayName makes a tmux session name safe to print: control characters and
// invalid bytes are shown as escapes, so a hostile name cannot send terminal
// escape sequences. The name that is saved is never changed.
func DisplayName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	s := b.String()
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}
