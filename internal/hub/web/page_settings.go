package web

// Administration pages, part 2: the settings page (Story 6.8, R71, R72). It
// lists every Hub setting with its environment name. A setting that the
// environment sets is shown greyed and read-only ("set by environment
// variable", AD-19); a secret is never shown back, only "set" or "not set".

import (
	"net/http"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Labels of the read-only state of a setting.
const (
	// EnvLockedText is the text of a field that the environment sets (R72).
	EnvLockedText = "set by environment variable"
	// panelLockedText is the text of a field that the panel can never change.
	panelLockedText = "cannot be changed in the panel"
)

// SettingsPage is what settings.html sees as .Data.
type SettingsPage struct {
	Settings []SettingRow
}

// SettingRow is one setting as the page shows it.
type SettingRow struct {
	Key  string
	Help string
	// Env is the environment variable; EnvFile the "_FILE" variable of a secret.
	Env     string
	EnvFile string
	Secret  bool
	// Value is the text of the input. It is empty for a secret, whatever the
	// operation returned: a secret is never rendered.
	Value   string
	Choices []string
	// SecretState is "set" or "not set" for a secret, empty otherwise.
	SecretState string
	// Source is "env", "panel" or "default".
	Source string
	// Locked says that the panel cannot change the setting; LockedText is why.
	Locked     bool
	LockedText string
	Live       bool
	// PendingRestart says that the stored value takes effect at the next start.
	PendingRestart bool
	// Error is why the effective value does not resolve, when it does not.
	Error string
}

// settingRows turns the result of settings_list into the rows of the page.
func settingRows(list []ops.SettingInfo) []SettingRow {
	rows := make([]SettingRow, 0, len(list))
	for _, s := range list {
		row := SettingRow{
			Key: s.Key, Help: s.Help, Env: s.Env, EnvFile: s.EnvFile, Secret: s.Secret,
			Choices: s.Choices, Source: s.Source, Locked: s.ReadOnly, Live: s.Live,
			PendingRestart: s.PendingRestart, Error: s.Error,
		}
		if s.Secret {
			row.SecretState = "not set"
			if s.Source == ops.SettingSourceEnv || s.Source == ops.SettingSourcePanel {
				row.SecretState = "set"
			}
		} else {
			row.Value = s.Value
		}
		if s.ReadOnly {
			row.LockedText = panelLockedText
			if s.Source == ops.SettingSourceEnv {
				row.LockedText = EnvLockedText
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func settingsData(r *http.Request, c OpCaller) (any, error) {
	var res ops.SettingsListResult
	if err := c.CallOp(r, ops.OpSettingsList, ops.SettingsListArgs{}, &res); err != nil {
		return nil, err
	}
	return SettingsPage{Settings: settingRows(res.Settings)}, nil
}

// handleSettingSet changes one setting. The form carries "value" and "action":
// "set" (the default) or "unset", which removes the panel value so that the
// default applies again. An empty value counts as unset in the operation, so a
// secret needs the explicit "unset" to be cleared: an empty field must not
// wipe a secret by accident.
func handleSettingSet(w http.ResponseWriter, r *http.Request, c OpCaller) {
	key := r.PathValue("key")
	value := r.PostFormValue("value")
	unset := r.PostFormValue("action") == "unset"
	if !unset && r.PostFormValue("secret") == "1" && strings.TrimSpace(value) == "" {
		writeResultError(w, "Enter a value, or use Remove to clear the secret.")
		return
	}
	args := ops.SettingsSetArgs{WriteArgs: newWriteArgs(), Key: key, Value: value, Unset: unset}
	if unset {
		args.Value = ""
	}
	text := "The setting " + key + " is saved."
	if unset {
		text = "The setting " + key + " is back to its default."
	}
	writeOp(w, r, c, ops.OpSettingsSet, args, text)
}

func init() {
	RegisterPage(Page{Pattern: pathAdminSettings, Title: "Hub settings", Template: "settings.html", Data: adminPageData(settingsData)})
	RegisterNav(NavItem{Label: "Hub settings", Path: pathAdminSettings, Order: adminNavOrder + 10, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-settings"})
	RegisterRoute(Route{Pattern: "POST " + pathAdminSettings + "/{key}", Handler: adminRoute(handleSettingSet)})
}
