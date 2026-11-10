package config

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// KeyInviteKey is the key of the Worker's invitation key setting (Story 4.6).
const KeyInviteKey = "invite_key"

// InviteKeyPrefix starts every invitation key (AD-7): mnk_<fingerprint>.<secret>.
const InviteKeyPrefix = "mnk_"

// InviteKeyArgvWarning is the one line printed to stderr when the invitation
// key is given literally on the command line (R85, AD-19).
const InviteKeyArgvWarning = "warning: the invitation key given on the command line is visible in the process list; " +
	"prefer --invite-key <file> or - (stdin)"

func init() { declareWorkerInvite(Default) }

// declareWorkerInvite declares invite_key: flag --invite-key, environment form
// MINION_INVITE_KEY_FILE (the path of a file that holds the key; the plain
// MINION_INVITE_KEY is rejected by the registry like for every secret).
//
// It is the one secret with a flag (ArgvAllowedWithWarning, R85): the flag
// value is the literal key (starts with mnk_), "-" for stdin or a file path,
// which [ResolveWorkerInviteKey] tells apart.
func declareWorkerInvite(r *Registry) {
	r.Declare(Setting{
		Key:    KeyInviteKey,
		Scope:  ScopeWorker,
		Secret: true,
		Help: "Invitation key (mnk_...) that enrolls this Worker at the Hub given by --server: the key itself, " +
			"a file that holds it, or - to read it from stdin. A literal key is visible in the process list. " +
			"The environment form is MINION_INVITE_KEY_FILE",
		ArgvAllowedWithWarning: true,
	})
}

// InviteKeyForm says how the user gave the invitation key.
type InviteKeyForm string

const (
	// InviteKeyLiteral is the key itself, as the flag value or in the config
	// file.
	InviteKeyLiteral InviteKeyForm = "literal"
	// InviteKeyStdin is "-": the key is the first line of stdin.
	InviteKeyStdin InviteKeyForm = "stdin"
	// InviteKeyPath is a file whose first line is the key (the flag value or
	// MINION_INVITE_KEY_FILE).
	InviteKeyPath InviteKeyForm = "path"
)

// ClassifyInviteKeyArg tells the forms of the --invite-key value apart: a value
// that starts with "mnk_" is the literal key, "-" is stdin and anything else
// is a file path.
func ClassifyInviteKeyArg(arg string) InviteKeyForm {
	switch {
	case strings.HasPrefix(arg, InviteKeyPrefix):
		return InviteKeyLiteral
	case arg == "-":
		return InviteKeyStdin
	}
	return InviteKeyPath
}

// InviteKeyInput is the resolved invitation key of a Worker run.
type InviteKeyInput struct {
	// Set is false when no invitation key was given.
	Set bool
	// Value is the key text, ready for the invitation parser. It is a secret:
	// the type never prints it.
	Value string
	// Source is where the setting came from.
	Source Source
	// Form is how the key was given.
	Form InviteKeyForm
	// Warning is [InviteKeyArgvWarning] when Form is the literal command-line
	// form, else empty. The caller prints it once to stderr.
	Warning string
}

// String implements fmt.Stringer and never reveals the key.
func (k InviteKeyInput) String() string {
	if !k.Set {
		return "invite_key=(unset)"
	}
	return fmt.Sprintf("invite_key=%s (%s, %s)", Redacted, k.Source, k.Form)
}

// GoString implements fmt.GoStringer so that "%#v" never prints the key.
func (k InviteKeyInput) GoString() string { return k.String() }

// ResolveWorkerInviteKey resolves invite_key with res (a Worker resolver) and
// reads the key it points to:
//
//   - --invite-key mnk_...  the literal key, with [InviteKeyArgvWarning];
//   - --invite-key -        the first line of stdin, no warning;
//   - --invite-key <path>   the first line of the file, no warning;
//   - MINION_INVITE_KEY_FILE=<path>, resolved by res: the first line of the
//     file, no warning;
//   - invite_key in the config file: the literal key, no warning.
//
// The first line is trimmed. No error and no warning ever repeats the key; a
// value that is neither a key nor a readable file is reported without being
// echoed unless it clearly is a path.
func ResolveWorkerInviteKey(res *Resolver, stdin io.Reader) (InviteKeyInput, error) {
	return ResolveWorkerInviteKeyContext(context.Background(), res, stdin)
}

// InviteKeyGiven reports whether an invitation key is configured, and whether
// it is the literal command-line form, without reading any file or stdin: it
// looks at the flag, at MINION_INVITE_KEY_FILE being non-empty and at the
// config file. An enrolled Worker uses it to ignore the key without touching
// its source (R94, AD-9). The plain MINION_INVITE_KEY is not looked at; it is
// rejected when the key is resolved.
func InviteKeyGiven(res *Resolver) (given, literalFlag bool, err error) {
	s, ok := res.registry().Lookup(KeyInviteKey)
	if !ok {
		return false, false, fmt.Errorf("unknown setting %q", KeyInviteKey)
	}
	if res.Role == ScopeWorker && s.HasFlag() && res.Flags != nil {
		v, ok, err := res.Flags.Lookup(KeyInviteKey)
		if err != nil {
			return false, false, fmt.Errorf("setting %q: flag: %w", KeyInviteKey, err)
		}
		if ok {
			// Like Resolve: an empty flag value means "no key" and the lower
			// sources are not consulted.
			return v != "", v != "" && ClassifyInviteKeyArg(v) == InviteKeyLiteral, nil
		}
	}
	if path, ok := res.lookupEnv(s.FileEnvName()); ok && path != "" {
		return true, false, nil
	}
	if res.File != nil {
		v, ok, err := res.File.Lookup(KeyInviteKey)
		if err != nil {
			return false, false, fmt.Errorf("setting %q: %s: %w", KeyInviteKey, SourceFile, err)
		}
		return ok && v != "", false, nil
	}
	return false, false, nil
}

// ResolveWorkerInviteKeyContext is [ResolveWorkerInviteKey] whose wait for
// stdin ends when ctx ends, so that SIGINT or SIGTERM interrupts a Worker that
// waits for a key that never comes.
func ResolveWorkerInviteKeyContext(ctx context.Context, res *Resolver, stdin io.Reader) (InviteKeyInput, error) {
	r, err := res.Resolve(KeyInviteKey)
	if err != nil {
		return InviteKeyInput{}, err
	}
	if r.Value == "" {
		return InviteKeyInput{}, nil
	}
	in := InviteKeyInput{Set: true, Source: r.Source, Form: InviteKeyLiteral}
	switch r.Source {
	case SourceFlag:
		in.Form = ClassifyInviteKeyArg(r.Value)
		switch in.Form {
		case InviteKeyLiteral:
			in.Value = strings.TrimSpace(r.Value)
			in.Warning = InviteKeyArgvWarning
			return in, nil
		case InviteKeyStdin:
			in.Value, err = readInviteKeyLineContext(ctx, stdin)
			if err != nil {
				return InviteKeyInput{}, fmt.Errorf("--%s -: %w", flagNameOf(KeyInviteKey), err)
			}
			return in, nil
		}
		in.Value, err = readInviteKeyFile(r.Value)
		if err != nil {
			return InviteKeyInput{}, fmt.Errorf("--%s: %w", flagNameOf(KeyInviteKey), err)
		}
		return in, nil
	case SourceEnv:
		// The resolver already read the file named by MINION_INVITE_KEY_FILE.
		in.Form = InviteKeyPath
	}
	in.Value, err = readInviteKeyLine(strings.NewReader(r.Value))
	if err != nil {
		return InviteKeyInput{}, fmt.Errorf("setting %q (%s): %w", KeyInviteKey, r.Source, err)
	}
	return in, nil
}

func flagNameOf(key string) string { return strings.ReplaceAll(key, "_", "-") }

// readInviteKeyLineContext is [readInviteKeyLine] that gives up when ctx ends.
// A blocked read cannot be interrupted, so it finishes in a goroutine that is
// left behind; the Worker is shutting down when that happens.
func readInviteKeyLineContext(ctx context.Context, r io.Reader) (string, error) {
	if r == nil {
		return readInviteKeyLine(r)
	}
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := readInviteKeyLine(r)
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// readInviteKeyLine returns the first line of r, trimmed.
func readInviteKeyLine(r io.Reader) (string, error) {
	if r == nil {
		return "", errors.New("no input to read the invitation key from")
	}
	line, err := bufio.NewReader(io.LimitReader(r, maxSecretFileSize)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("cannot read the invitation key: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("the invitation key is empty")
	}
	return line, nil
}

// readInviteKeyFile reads the first line of the file at path. The value of
// --invite-key may be a mistyped key, so the error names it only when it looks
// like a path.
func readInviteKeyFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		var pe *fs.PathError
		reason := err
		if errors.As(err, &pe) {
			reason = pe.Err
		}
		if strings.ContainsAny(path, `/\`) || strings.HasPrefix(path, ".") {
			return "", fmt.Errorf("cannot read the invitation key file %q: %w", path, reason)
		}
		return "", fmt.Errorf("the value is not an invitation key (it must start with %s) and is not a readable file: %w",
			InviteKeyPrefix, reason)
	}
	defer func() { _ = f.Close() }()
	v, err := readInviteKeyLine(f)
	if err != nil {
		return "", fmt.Errorf("invitation key file %q: %w", path, err)
	}
	return v, nil
}
