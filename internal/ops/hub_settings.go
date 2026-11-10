package ops

import (
	"reflect"
	"unicode/utf8"
)

// Names of the Hub settings operations (AD-2, AD-19).
const (
	OpSettingsList = "settings_list"
	OpSettingsSet  = "settings_set"
)

// MaxSettingValueLen caps the value of one panel setting in bytes.
const MaxSettingValueLen = 8192

// Setting sources reported by settings_list. They mirror the precedence of
// the Hub (AD-19): environment, then the panel, then the default.
const (
	SettingSourceEnv     = "env"
	SettingSourcePanel   = "panel"
	SettingSourceDefault = "default"
)

// Warning codes of settings_set.
const (
	// WarnRestartRequired says the value is stored but takes effect only
	// after the next Hub restart, because the setting is not live.
	WarnRestartRequired = "restart_required"
)

// guardSettingNotEnv refuses to change a setting that the environment sets
// (R72). It is KEEP: force does not override the environment.
var guardSettingNotEnv = Guard{Name: "setting_not_env", Class: GuardKeep, Reads: "settings.source"}

// SettingsListArgs are the arguments of settings_list (none).
type SettingsListArgs struct{}

// SettingInfo describes one Hub setting for the panel.
type SettingInfo struct {
	Key  string `json:"key"`
	Help string `json:"help,omitempty"`
	// Env is the environment variable that sets the value; EnvFile is the
	// "<NAME>_FILE" variable of a secret.
	Env     string `json:"env"`
	EnvFile string `json:"env_file,omitempty"`
	// Value is the effective value, "[redacted]" for a secret that is set.
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
	// Source is env, panel or default.
	Source string `json:"source"`
	// ReadOnly is true when the environment sets the value (R72); the panel
	// shows the label "set by environment variable".
	ReadOnly bool `json:"read_only"`
	// Live is true when a change applies without a restart.
	Live bool `json:"live"`
	// PendingRestart is true when a stored panel value differs from the one
	// in effect because the setting is not live.
	PendingRestart bool     `json:"pending_restart"`
	Choices        []string `json:"choices,omitempty"`
	// Error is set when the setting cannot be resolved, for example because
	// the environment holds an invalid value. It never contains a secret.
	Error string `json:"error,omitempty"`
}

// SettingsListResult is the result of settings_list, ordered by key.
type SettingsListResult struct {
	Settings []SettingInfo `json:"settings"`
}

// SettingsSetArgs are the arguments of settings_set.
type SettingsSetArgs struct {
	WriteArgs
	// Key is the setting key as listed by settings_list.
	Key string `json:"key"`
	// Value is the new panel value. For a secret it is encrypted at rest and
	// never echoed back.
	Value string `json:"value,omitempty"`
	// Unset removes the panel value so that the default applies again. It
	// excludes Value.
	Unset bool `json:"unset,omitempty"`
}

// Validate checks the request id, the key shape, the value size and that a
// value and unset are not combined. Whether the key exists and whether the
// value is acceptable for it is decided on the Hub, against the registry.
func (a SettingsSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !isSnakeCase(a.Key) {
		return invalid("key", "must be a snake_case setting key")
	}
	if a.Unset && a.Value != "" {
		return invalid("value", "must be empty when unset is true")
	}
	if len(a.Value) > MaxSettingValueLen {
		return invalid("value", "must be at most %d bytes", MaxSettingValueLen)
	}
	if !utf8.ValidString(a.Value) {
		return invalid("value", "must be valid UTF-8")
	}
	return nil
}

func init() {
	manage := MustActionSet(ActionManage)

	Register(Op{
		Name: OpSettingsList, Summary: "List the Hub settings with their effective value, source and whether the panel may change them.",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[SettingsListArgs](), Result: reflect.TypeFor[SettingsListResult](),
	})
	Register(Op{
		Name: OpSettingsSet, Summary: "Set or unset a Hub setting in the panel unless the environment sets it.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardSettingNotEnv},
		Args:   reflect.TypeFor[SettingsSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
