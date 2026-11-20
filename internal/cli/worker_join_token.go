package cli

// The join token of "minion worker up --join <token>" (v0.12.0). The Hub's
// "Add worker" page shows one string, mnj1-..., that holds the Hub URL and a
// one-time invitation key whose fingerprint is the full Hub fingerprint (see
// identity.JoinToken). The CLI expands it before anything else resolves a
// setting: it becomes --server, --hub-fingerprint and --invite-key of the
// command, so the existing code paths (the first-run gate, the enrollment
// hook with its fingerprint check, the config file that the service reads,
// the quarantine of a paired Worker whose Hub differs) apply unchanged.
//
// The token is read from --join, MINION_JOIN or MINION_JOIN_FILE, never from
// the config file. A setting given by flag or environment that names a
// different Hub, or a second invitation key, is an error that names both
// sources: the token is never silently mixed with other values. The token is
// a secret: it is never printed, logged or put into an error.

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// joinAppliedAnnotation marks a command whose join token was expanded, so that
// "worker up" in the foreground and the "worker run" it calls expand once.
const joinAppliedAnnotation = "minion.join-applied"

// joinExpansion is the outcome of [expandJoinToken].
type joinExpansion struct {
	// Applied is true when a join token set the Hub settings of the command.
	Applied bool
	// Warning is the process-list warning of a token given as --join, returned
	// only by the call that expanded it.
	Warning string
}

// joinHint is the way to a join token from the commands that do not take one.
const joinHint = "a join token (mnj1-...) enrolls with `minion worker up --join <token>`"

// addJoinFlag adds --join to a command that expands it.
func addJoinFlag(cmd *cobra.Command, reg *config.Registry) {
	s, ok := reg.Lookup(config.KeyJoin)
	if !ok || cmd.Flags().Lookup(s.FlagName()) != nil {
		return
	}
	cmd.Flags().String(s.FlagName(), "", fmt.Sprintf("%s (env %s; --%s - reads the token from standard input, so it is in no argument list)", s.Help, s.EnvName(), s.FlagName()))
}

// expandJoinToken reads the join token of cmd and, when there is one, sets the
// flags of server, hub_fingerprint and invite_key from it. It does nothing
// when the command has no --join flag or no token is given.
func expandJoinToken(cmd *cobra.Command, reg *config.Registry, lookupEnv func(string) (string, bool)) (joinExpansion, error) {
	if cmd.Annotations[joinAppliedAnnotation] != "" {
		return joinExpansion{Applied: true}, nil
	}
	js, ok := reg.Lookup(config.KeyJoin)
	if !ok || cmd.Flags().Lookup(js.FlagName()) == nil {
		return joinExpansion{}, nil
	}
	// Flags and environment only: the token is never read from the config
	// file, and the values it is compared with are this run's explicit ones.
	res := &config.Resolver{Registry: reg, Role: config.ScopeWorker, Flags: changedFlags{cmd: cmd, reg: reg}, LookupEnv: lookupEnv}
	// A token pasted where the path of a file belongs: the read error would
	// name the "path", which is the secret.
	if v, ok := lookupEnv(js.FileEnvName()); ok && identity.LooksLikeJoinToken(v) {
		return joinExpansion{}, fmt.Errorf("%s holds a join token, but it takes the path of a file that holds one; "+
			"put the token in %s, or in a private file and set %s to that file", js.FileEnvName(), js.EnvName(), js.FileEnvName())
	}
	r, err := res.Resolve(config.KeyJoin)
	if err != nil {
		return joinExpansion{}, err
	}
	if r.Source == config.SourceDefault {
		return joinExpansion{}, nil
	}
	from := sourceName(js, r)
	if r.Source == config.SourceFlag && strings.TrimSpace(r.Value) == "-" {
		// --join - reads the token from standard input, so that it is in no
		// argument list (the way a program, such as the tray, hands it over).
		line, err := readJoinTokenLine(cmd.InOrStdin())
		if err != nil {
			return joinExpansion{}, fmt.Errorf("--%s -: %w", js.FlagName(), err)
		}
		r.Value, r.Warning = line, ""
	}
	if strings.TrimSpace(r.Value) == "" {
		// Given, but blank: a quoted empty --join or a file of white space
		// is a mistake, never "no token".
		return joinExpansion{}, fmt.Errorf("%s: malformed join token: it is empty; copy the whole token from the Hub's Add worker page", from)
	}
	tok, err := identity.ParseJoinToken(r.Value)
	if err != nil {
		return joinExpansion{}, fmt.Errorf("%s: %w; copy the whole token from the Hub's Add worker page", from, err)
	}

	if err := joinServerConflict(res, from, tok); err != nil {
		return joinExpansion{}, err
	}
	if err := joinFingerprintConflict(res, from, tok); err != nil {
		return joinExpansion{}, err
	}
	if given, _, err := config.InviteKeyGiven(res); err != nil {
		return joinExpansion{}, err
	} else if given {
		ks, _ := reg.Lookup(config.KeyInviteKey)
		return joinExpansion{}, fmt.Errorf("%s already holds a one-time invitation key, and --%s or %s gives another one; give only one of them",
			from, ks.FlagName(), ks.FileEnvName())
	}

	for key, value := range map[string]string{
		config.KeyServer:         tok.HubURL,
		config.KeyHubFingerprint: tok.Fingerprint().String(),
		config.KeyInviteKey:      tok.Invite.String(),
	} {
		s, ok := reg.Lookup(key)
		if !ok {
			return joinExpansion{}, fmt.Errorf("setting %q is not declared", key)
		}
		if err := cmd.Flags().Set(s.FlagName(), value); err != nil {
			// The error of a flag value could repeat it: name the flag only.
			return joinExpansion{}, fmt.Errorf("cannot apply the join token to --%s", s.FlagName())
		}
	}
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[joinAppliedAnnotation] = "1"
	return joinExpansion{Applied: true, Warning: r.Warning}, nil
}

// maxJoinTokenInput bounds what --join - reads: a join token is about 250
// characters and identity.ParseJoinToken refuses more than 4096.
const maxJoinTokenInput = 4096

// readJoinTokenLine reads the first line of r, one byte at a time so that
// nothing after it is taken (the wizard may read the next answer from the same
// input), and at most maxJoinTokenInput bytes. The error never holds the input.
func readJoinTokenLine(r io.Reader) (string, error) {
	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			if len(buf) >= maxJoinTokenInput {
				return "", fmt.Errorf("the input is longer than %d bytes; it is not a join token", maxJoinTokenInput)
			}
			buf = append(buf, one[0])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("cannot read the join token: %w", err)
		}
	}
	line := strings.TrimSpace(string(buf))
	if line == "" {
		return "", errors.New("the input holds no join token; copy the whole token from the Hub's Add worker page")
	}
	return line, nil
}

// sourceName names where a resolved value came from: the flag, or the
// environment variable that was read.
func sourceName(s config.Setting, r config.Resolved) string {
	switch r.Source {
	case config.SourceFlag:
		return "--" + s.FlagName()
	case config.SourceEnv:
		if s.Secret {
			return s.EnvName() + " (or " + s.FileEnvName() + ")"
		}
		return s.EnvName()
	}
	return string(r.Source)
}

// joinServerConflict fails when --server or MINION_SERVER names another Hub
// address than the token. Equal addresses (scheme and host in any letter
// case, a trailing slash) are not a conflict.
func joinServerConflict(res *config.Resolver, from string, tok identity.JoinToken) error {
	r, err := res.Resolve(config.KeyServer)
	if err != nil {
		return err
	}
	if r.Source == config.SourceDefault {
		return nil
	}
	if strings.TrimSpace(r.Value) == "" {
		// An explicitly empty --server selects Solo: not a Hub at all.
		return fmt.Errorf("%s names the Hub %s, but %s is empty (no Hub, Solo); give only one of them",
			from, safeURL(tok.HubURL), sourceName(r.Setting, r))
	}
	given, err := config.ValidateServerURL(r.Value)
	if err != nil {
		return fmt.Errorf("setting %q (%s): %w", config.KeyServer, r.Source, err)
	}
	if sameHubURL(given, tok.HubURL) {
		return nil
	}
	return fmt.Errorf("%s names the Hub %s, but %s names %s; give only one of them",
		from, safeURL(tok.HubURL), sourceName(r.Setting, r), safeURL(given))
}

// safeURL is a Hub URL for a message: scheme, host and path, without a user
// name, password, query or fragment, which may carry a credential.
func safeURL(v string) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Host == "" {
		return "(an address that is not a valid URL)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// joinFileConflict compares the Hub of an expanded join token (now the flags
// of cmd) with the Hub address and fingerprint saved in the Worker's config
// file. A fingerprint provisioned there beforehand is a pin the token must
// not replace: a different one is an error, before anything dials or writes.
// A different saved address is an error too. file may be nil.
func joinFileConflict(cmd *cobra.Command, reg *config.Registry, file config.FileSource) error {
	if cmd.Annotations[joinAppliedAnnotation] == "" || file == nil {
		return nil
	}
	flag := func(key string) string {
		s, _ := reg.Lookup(key)
		if f := cmd.Flags().Lookup(s.FlagName()); f != nil {
			return f.Value.String()
		}
		return ""
	}
	const from = "the join token (--join, MINION_JOIN or MINION_JOIN_FILE)"
	fix := "; to move this Worker to the Hub of the token, run `minion worker config unset server hub_fingerprint` first"
	if v, ok, err := file.Lookup(config.KeyHubFingerprint); err != nil {
		return fmt.Errorf("setting %q (%s): %w", config.KeyHubFingerprint, config.SourceFile, err)
	} else if ok && strings.TrimSpace(v) != "" {
		m, err := agent.ParseHubExpectation(v)
		if err != nil {
			return fmt.Errorf("the config file: %w", err)
		}
		tokFp, err := identity.ParseFingerprint(flag(config.KeyHubFingerprint))
		if err != nil || !m.MatchesFingerprint(tokFp) {
			return fmt.Errorf("%s names the Hub fingerprint %s, but hub_fingerprint in the config file names %s; nothing was sent%s",
				from, tokFp.Words(), m.Words(), fix)
		}
	}
	if v, ok, err := file.Lookup(config.KeyServer); err != nil {
		return fmt.Errorf("setting %q (%s): %w", config.KeyServer, config.SourceFile, err)
	} else if ok && strings.TrimSpace(v) != "" && !sameHubURL(v, flag(config.KeyServer)) {
		return fmt.Errorf("%s names the Hub %s, but server in the config file names %s; nothing was sent%s",
			from, safeURL(flag(config.KeyServer)), safeURL(v), fix)
	}
	return nil
}

// noArgsRedacted is cobra.NoArgs that never repeats an argument that looks
// like a secret (a join token or an invitation key pasted without its flag).
func noArgsRedacted(cmd *cobra.Command, args []string) error {
	for _, a := range args {
		if identity.LooksLikeJoinToken(a) {
			return fmt.Errorf("%q takes no arguments; a join token is given as --join <token> (or MINION_JOIN)", cmd.CommandPath())
		}
		if strings.HasPrefix(strings.TrimSpace(a), config.InviteKeyPrefix) {
			return fmt.Errorf("%q takes no arguments; an invitation key is given as --invite-key <file>", cmd.CommandPath())
		}
	}
	return cobra.NoArgs(cmd, args)
}

// sameHubURL compares two Hub URLs: scheme and host without regard to letter
// case, the path without a trailing slash.
func sameHubURL(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}

// joinFingerprintConflict fails when --hub-fingerprint or
// MINION_HUB_FINGERPRINT names another Hub than the token. Six words that
// match the token's fingerprint are not a conflict.
func joinFingerprintConflict(res *config.Resolver, from string, tok identity.JoinToken) error {
	r, err := res.Resolve(config.KeyHubFingerprint)
	if err != nil {
		return err
	}
	if r.Source == config.SourceDefault || strings.TrimSpace(r.Value) == "" {
		return nil
	}
	m, err := agent.ParseHubExpectation(r.Value)
	if err != nil {
		return fmt.Errorf("%s: %w", sourceName(r.Setting, r), err)
	}
	if m.MatchesFingerprint(tok.Fingerprint()) {
		return nil
	}
	return fmt.Errorf("%s names the Hub fingerprint %s, but %s names %s; give only one of them",
		from, tok.Fingerprint().Words(), sourceName(r.Setting, r), m.Words())
}
