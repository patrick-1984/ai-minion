package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// EnvPrefix starts the environment name of every setting.
const EnvPrefix = "MINION_"

// FileSuffix is appended to a secret setting's environment name to name the
// variable that holds the path of the file containing the secret.
const FileSuffix = "_FILE"

// Scope says which role resolves a setting, and therefore which precedence
// chain applies.
type Scope string

const (
	// ScopeHub settings are resolved by the Hub: env, panel, default.
	ScopeHub Scope = "hub"
	// ScopeWorker settings are resolved by the Worker: flag, env, config
	// file, default.
	ScopeWorker Scope = "worker"
	// ScopeAny settings are declared for both roles (for example the log
	// level). They are resolved by whichever role asks, with that role's
	// precedence chain.
	ScopeAny Scope = "any"
)

func (s Scope) valid() bool {
	return s == ScopeHub || s == ScopeWorker || s == ScopeAny
}

// appliesTo reports whether a setting of scope s can be resolved by role.
func (s Scope) appliesTo(role Scope) bool {
	return s == ScopeAny || s == role
}

var (
	keyRE = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	envRE = regexp.MustCompile(`^MINION_[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)
)

// Setting declares one setting. Only Key and Scope are required.
type Setting struct {
	// Key is the lower_snake_case name used in the config file and the panel,
	// for example "data_dir".
	Key string
	// Scope selects the precedence chain.
	Scope Scope
	// Env overrides the environment name. It is normally empty, which means
	// "MINION_" plus the upper-cased Key. It must start with MINION_, be
	// UPPER_SNAKE_CASE and must not end in _FILE.
	Env string
	// Help is a one-line English description for flag help and the panel.
	Help string
	// Default is the static default value.
	Default string
	// DefaultFunc computes the default (for OS-dependent paths). It is used
	// when Default is empty. An error makes resolution of the setting fail.
	DefaultFunc func() (string, error)
	// Choices, when not empty, lists the only accepted values.
	Choices []string

	// Secret marks a value that must never be logged, printed or passed in
	// argv. A secret is readable only from "<ENV>_FILE", the panel or the
	// config file, and a secret has no default.
	Secret bool
	// PlainEnvAllowed is the explicitly declared exception that lets a
	// secret be read from its plain environment variable. The only
	// exception in release 1 is MINION_ADMIN_PASSWORD (the one-time seed,
	// AD-10).
	PlainEnvAllowed bool
	// ArgvAllowedWithWarning is the explicitly declared exception that gives
	// a secret a flag whose literal value may be passed in argv (Story 4.6
	// declares it for --invite-key). A resolved value from the flag carries a
	// warning for the CLI to print.
	ArgvAllowedWithWarning bool
}

// EnvName returns the environment name of the setting.
func (s Setting) EnvName() string {
	if s.Env != "" {
		return s.Env
	}
	return EnvPrefix + strings.ToUpper(s.Key)
}

// FileEnvName returns the name of the variable that holds the path of a file
// with the setting's value, "<ENV>_FILE". Only secret settings honour it.
func (s Setting) FileEnvName() string { return s.EnvName() + FileSuffix }

// FlagName returns the flag name without the leading dashes: the environment
// name without the MINION_ prefix, lower-cased, with dashes for underscores.
func (s Setting) FlagName() string {
	name := strings.TrimPrefix(s.EnvName(), EnvPrefix)
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// HasFlag reports whether the setting has a command-line flag. Secrets have
// none unless ArgvAllowedWithWarning declares the exception, and the Hub has
// no flags at all (the Hub scope chain does not read them).
func (s Setting) HasFlag() bool {
	if s.Scope == ScopeHub {
		return false
	}
	return !s.Secret || s.ArgvAllowedWithWarning
}

func (s Setting) validate() error {
	if !keyRE.MatchString(s.Key) {
		return fmt.Errorf("key %q is not lower_snake_case", s.Key)
	}
	if !s.Scope.valid() {
		return fmt.Errorf("setting %q has invalid scope %q", s.Key, s.Scope)
	}
	env := s.EnvName()
	if !strings.HasPrefix(env, EnvPrefix) {
		return fmt.Errorf("setting %q: env name %q does not start with %s", s.Key, env, EnvPrefix)
	}
	if !envRE.MatchString(env) {
		return fmt.Errorf("setting %q: env name %q is not UPPER_SNAKE_CASE", s.Key, env)
	}
	if strings.HasSuffix(env, FileSuffix) {
		return fmt.Errorf("setting %q: env name %q must not end in %s", s.Key, env, FileSuffix)
	}
	if s.Secret && (s.Default != "" || s.DefaultFunc != nil) {
		return fmt.Errorf("setting %q: a secret must not have a default", s.Key)
	}
	if !s.Secret && (s.PlainEnvAllowed || s.ArgvAllowedWithWarning) {
		return fmt.Errorf("setting %q: secret exceptions apply to secret settings only", s.Key)
	}
	if s.Default != "" && s.DefaultFunc != nil {
		return fmt.Errorf("setting %q: Default and DefaultFunc are mutually exclusive", s.Key)
	}
	if len(s.Choices) > 0 && s.Default != "" && !contains(s.Choices, s.Default) {
		return fmt.Errorf("setting %q: default %q is not one of %v", s.Key, s.Default, s.Choices)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, c := range list {
		if c == v {
			return true
		}
	}
	return false
}

// Registry holds the declared settings. The zero value is not usable; use
// [NewRegistry]. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	byKey map[string]Setting
	byEnv map[string]string // env name -> key
	byFlg map[string]string // flag name -> key
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byKey: map[string]Setting{},
		byEnv: map[string]string{},
		byFlg: map[string]string{},
	}
}

// Default is the process-wide registry that the packages of the binary
// declare their settings in, from init functions.
var Default = NewRegistry()

// Declare adds a setting to the process-wide registry. See [Registry.Declare].
func Declare(s Setting) { Default.Declare(s) }

// Declare adds a setting. It panics on an invalid declaration (bad key, scope
// or env name, a secret with a default) and on a duplicate key, environment
// name or flag name, because a wrong declaration is a programming error that
// must fail at init, not in production.
func (r *Registry) Declare(s Setting) {
	if err := s.validate(); err != nil {
		panic("config: " + err.Error())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byKey[s.Key]; dup {
		panic(fmt.Sprintf("config: duplicate setting key %q", s.Key))
	}
	env := s.EnvName()
	if other, dup := r.byEnv[env]; dup {
		panic(fmt.Sprintf("config: setting %q: env name %s is already used by %q", s.Key, env, other))
	}
	flag := s.FlagName()
	if other, dup := r.byFlg[flag]; dup {
		panic(fmt.Sprintf("config: setting %q: flag --%s is already used by %q", s.Key, flag, other))
	}
	r.byKey[s.Key] = s
	r.byEnv[env] = s.Key
	r.byFlg[flag] = s.Key
}

// Lookup returns the setting declared under key.
func (r *Registry) Lookup(key string) (Setting, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byKey[key]
	return s, ok
}

// All returns every declared setting ordered by key.
func (r *Registry) All() []Setting {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Setting, 0, len(r.byKey))
	for _, s := range r.byKey {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ForScope returns the settings that role can resolve (its own scope and
// [ScopeAny]), ordered by key.
func (r *Registry) ForScope(role Scope) []Setting {
	var out []Setting
	for _, s := range r.All() {
		if s.Scope.appliesTo(role) {
			out = append(out, s)
		}
	}
	return out
}
