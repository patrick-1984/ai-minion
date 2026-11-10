package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Source says where a resolved value came from.
type Source string

const (
	// SourceFlag is a command-line flag (Worker only).
	SourceFlag Source = "flag"
	// SourceEnv is the environment: the plain variable, or for a secret the
	// "<NAME>_FILE" variable.
	SourceEnv Source = "env"
	// SourceFile is the Worker config file.
	SourceFile Source = "file"
	// SourcePanel is the Hub panel (SQLite).
	SourcePanel Source = "panel"
	// SourceDefault is the declared default (or no value at all).
	SourceDefault Source = "default"
)

// ValueSource is a lookup of raw setting values by key. ok is false when the
// source holds no value for the key.
type ValueSource interface {
	Lookup(key string) (value string, ok bool, err error)
}

// PanelSource is the Hub panel's settings store (Story 3.4 implements it on
// SQLite). Lookup must report ok=false for a key the panel has not set.
type PanelSource interface{ ValueSource }

// FlagSource reports the flags the user set on the command line. It must
// return only flags that were explicitly given, never flag defaults.
type FlagSource interface{ ValueSource }

// FileSource is the Worker config file.
type FileSource interface{ ValueSource }

// MapSource is a ValueSource over a map. It serves tests and simple callers.
type MapSource map[string]string

// Lookup implements ValueSource.
func (m MapSource) Lookup(key string) (string, bool, error) {
	v, ok := m[key]
	return v, ok, nil
}

// Resolved is the outcome of resolving one setting.
type Resolved struct {
	Setting Setting
	// Value is the effective value. It is empty when nothing set it and the
	// setting has no default. Use [Resolved.Display] for output.
	Value string
	// Source is where Value came from.
	Source Source
	// Warning is a one-line message the CLI should print, for example that a
	// secret was given in argv. Empty when there is nothing to warn about.
	Warning string
}

// Key returns the setting key.
func (r Resolved) Key() string { return r.Setting.Key }

// ReadOnly reports whether the panel must show the field read-only with the
// label "set by environment variable" (R72).
func (r Resolved) ReadOnly() bool { return r.Source == SourceEnv }

// Redacted is what output shows in place of a secret value.
const Redacted = "[redacted]"

// Display returns the value for human output: the value itself, or
// [Redacted] for a secret that is set.
func (r Resolved) Display() string {
	if r.Setting.Secret && r.Value != "" {
		return Redacted
	}
	return r.Value
}

// String implements fmt.Stringer and never reveals a secret.
func (r Resolved) String() string {
	return fmt.Sprintf("%s=%s (%s)", r.Setting.Key, r.Display(), r.Source)
}

// GoString implements fmt.GoStringer so that "%#v" never prints a secret.
func (r Resolved) GoString() string { return r.String() }

// LogValue implements slog.LogValuer: a Resolved logs its key, its display
// value (redacted for a secret) and its source, never the raw value.
func (r Resolved) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("key", r.Setting.Key),
		slog.String("value", r.Display()),
		slog.String("source", string(r.Source)),
	)
}

// Resolver resolves settings for one role. Fill the sources that apply to the
// role; the others are ignored, so a Hub resolver never reads flags or a
// config file and a Worker resolver never reads the panel.
type Resolver struct {
	// Registry is the registry to read; nil means [Default].
	Registry *Registry
	// Role is [ScopeHub] or [ScopeWorker].
	Role Scope

	// Flags, File (Worker) and Panel (Hub) may be nil.
	Flags FlagSource
	File  FileSource
	Panel PanelSource

	// LookupEnv defaults to [os.LookupEnv].
	LookupEnv func(name string) (string, bool)
	// ReadFile reads "<NAME>_FILE" targets; it defaults to a size-capped
	// os read.
	ReadFile func(path string) ([]byte, error)
}

func (r *Resolver) registry() *Registry {
	if r.Registry != nil {
		return r.Registry
	}
	return Default
}

func (r *Resolver) lookupEnv(name string) (string, bool) {
	if r.LookupEnv != nil {
		return r.LookupEnv(name)
	}
	return os.LookupEnv(name)
}

func (r *Resolver) readFile(path string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile(path)
	}
	return readLimited(path)
}

// Resolve returns the effective value of the setting declared under key.
func (r *Resolver) Resolve(key string) (Resolved, error) {
	if r.Role != ScopeHub && r.Role != ScopeWorker {
		return Resolved{}, fmt.Errorf("config: resolver role must be %q or %q, got %q", ScopeHub, ScopeWorker, r.Role)
	}
	s, ok := r.registry().Lookup(key)
	if !ok {
		return Resolved{}, fmt.Errorf("unknown setting %q", key)
	}
	if !s.Scope.appliesTo(r.Role) {
		return Resolved{}, fmt.Errorf("setting %q is a %s setting and cannot be resolved for the %s", key, s.Scope, r.Role)
	}

	res, found, err := r.fromChain(s)
	if err != nil {
		return Resolved{}, err
	}
	if !found {
		res = Resolved{Setting: s, Source: SourceDefault}
		switch {
		case s.DefaultFunc != nil:
			v, err := s.DefaultFunc()
			if err != nil {
				return Resolved{}, fmt.Errorf("setting %q: cannot compute default: %w", key, err)
			}
			res.Value = v
		default:
			res.Value = s.Default
		}
	}
	if len(s.Choices) > 0 && res.Value != "" && !contains(s.Choices, res.Value) {
		shown := fmt.Sprintf("%q", res.Value)
		if s.Secret {
			shown = Redacted
		}
		return Resolved{}, fmt.Errorf("setting %q (%s): invalid value %s, expected one of %s",
			key, res.Source, shown, strings.Join(s.Choices, ", "))
	}
	return res, nil
}

// ResolveAll resolves every setting the role can see, ordered by key. It
// returns all values that resolved, and the joined errors of those that did
// not.
func (r *Resolver) ResolveAll() ([]Resolved, error) {
	var out []Resolved
	var errs []error
	for _, s := range r.registry().ForScope(r.Role) {
		res, err := r.Resolve(s.Key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, res)
	}
	return out, errors.Join(errs...)
}

// fromChain walks the precedence chain of the role. found is false when no
// source holds a value.
func (r *Resolver) fromChain(s Setting) (res Resolved, found bool, err error) {
	res.Setting = s

	if r.Role == ScopeWorker && s.HasFlag() && r.Flags != nil {
		v, ok, err := r.Flags.Lookup(s.Key)
		if err != nil {
			return res, false, fmt.Errorf("setting %q: flag: %w", s.Key, err)
		}
		if ok {
			res.Value, res.Source = v, SourceFlag
			if s.Secret {
				res.Warning = fmt.Sprintf("warning: --%s was given on the command line; argv is visible in the process list", s.FlagName())
			}
			return res, true, nil
		}
	}

	v, ok, err := r.fromEnv(s)
	if err != nil {
		return res, false, err
	}
	if ok {
		res.Value, res.Source = v, SourceEnv
		return res, true, nil
	}

	var second ValueSource
	var secondSrc Source
	if r.Role == ScopeHub {
		if r.Panel != nil {
			second, secondSrc = r.Panel, SourcePanel
		}
	} else if r.File != nil {
		second, secondSrc = r.File, SourceFile
	}
	if second != nil {
		v, ok, err := second.Lookup(s.Key)
		if err != nil {
			return res, false, fmt.Errorf("setting %q: %s: %w", s.Key, secondSrc, err)
		}
		if ok {
			res.Value, res.Source = v, secondSrc
			return res, true, nil
		}
	}
	return res, false, nil
}

// fromEnv reads the environment for s. An empty variable counts as unset.
// For a secret it honours "<NAME>_FILE", rejects the plain variable unless
// the setting declares the exception, and never puts the secret in an error.
func (r *Resolver) fromEnv(s Setting) (string, bool, error) {
	plain, plainSet := r.lookupEnv(s.EnvName())
	plainSet = plainSet && plain != ""
	if !s.Secret {
		return plain, plainSet, nil
	}

	if plainSet && !s.PlainEnvAllowed {
		return "", false, fmt.Errorf("setting %q is a secret: %s is not accepted; put the value in a file and set %s to its path, or use the config file or the panel",
			s.Key, s.EnvName(), s.FileEnvName())
	}
	if path, ok := r.lookupEnv(s.FileEnvName()); ok && path != "" {
		data, err := r.readFile(path)
		if err != nil {
			return "", false, fmt.Errorf("setting %q: %s: cannot read secret file: %w", s.Key, s.FileEnvName(), err)
		}
		v, err := readSecretFile(func(string) ([]byte, error) { return data, nil }, path)
		if err != nil {
			return "", false, fmt.Errorf("setting %q: %s: %w", s.Key, s.FileEnvName(), err)
		}
		return v, true, nil
	}
	if plainSet {
		return plain, true, nil
	}
	return "", false, nil
}
