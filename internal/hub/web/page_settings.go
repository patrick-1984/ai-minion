package web

// Administration pages, part 2: the settings page (Story 6.8, R71, R72). It
// lists every Hub setting with its environment name. A setting that the
// environment sets is shown greyed and read-only ("set by environment
// variable", AD-19); a secret is never shown back, only "set" or "not set".

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/config"

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

// SettingsPage is what settings.html sees as .Data: the settings grouped in
// sections, in config.SectionOrder, with no empty section.
type SettingsPage struct {
	Sections []SettingSection
	// Sinks is the effective state of the remote log sinks, shown in the
	// Log export section; empty when hub_diagnostics did not answer.
	Sinks []DiagnosticsFact
}

// SettingSection is one card of the settings page.
type SettingSection struct {
	// Name is the heading; ID the fragment of the jump link.
	Name string
	ID   string
	Rows []SettingRow
	// Batch says that the section is saved as one change (settings_set with
	// changes): its settings only make sense together, such as a log sink's
	// switch, endpoint, credentials and trust.
	Batch bool
	// Compliance says that the section is saved with compliance_set (a fresh
	// sign-in of an administrator), not settings_set.
	Compliance bool
}

// pathAdminSettingsSection saves a batch section.
const pathAdminSettingsSection = "/admin/settings-section"

// batchSections are the sections saved as one change.
var batchSections = []string{config.SectionLogExport, config.SectionCompliance}

// pathAdminComplianceSection saves the compliance section.
const pathAdminComplianceSection = "/admin/compliance-section"

// SettingRow is one setting as the page shows it.
type SettingRow struct {
	Key string
	// Section is the card the row belongs to; rank orders the rows inside it.
	Section string
	rank    int
	Help    string
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
	// Revision is the stored value's revision; a batch save sends it back so
	// that a page loaded before someone else's change cannot undo it.
	Revision string
	// Multiline is a value of several lines (the OTLP header list).
	Multiline bool
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
			PendingRestart: s.PendingRestart, Error: s.Error, Revision: s.Revision,
			Multiline: s.Key == config.KeyOTelHeaders,
		}
		// The operation names the section; an older result without one
		// falls back to the local table.
		sec, rank := config.SectionOf(s.Key)
		row.Section, row.rank = s.Section, rank
		if row.Section == "" {
			row.Section = sec
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
	page := SettingsPage{Sections: settingSections(settingRows(res.Settings))}
	var d ops.HubDiagnosticsResult
	if err := c.CallOp(r, ops.OpHubDiagnostics, ops.HubDiagnosticsArgs{}, &d); err == nil {
		page.Sinks = sinkFacts(d.Sinks)
	}
	return page, nil
}

// settingSections groups rows by section in config.SectionOrder. Inside a
// section the essential settings (rank 0) come first, then the rest from A to
// Z. A section that the order does not know goes last, under its own name.
func settingSections(rows []SettingRow) []SettingSection {
	order := slices.Clone(config.SectionOrder)
	for _, r := range rows {
		if !slices.Contains(order, r.Section) {
			order = append(order, r.Section)
		}
	}
	var out []SettingSection
	for _, name := range order {
		var in []SettingRow
		for _, r := range rows {
			if r.Section == name {
				in = append(in, r)
			}
		}
		if len(in) == 0 {
			continue
		}
		slices.SortStableFunc(in, func(a, b SettingRow) int {
			if a.rank != b.rank {
				return a.rank - b.rank
			}
			return strings.Compare(a.Key, b.Key)
		})
		if name == config.SectionCompliance {
			// The payload level of the traffic log belongs here too: it records
			// session content and changes only through compliance_set.
			for _, r := range rows {
				if r.Key == config.KeyTrafficLog {
					in = append(in, r)
				}
			}
		}
		out = append(out, SettingSection{Name: name, ID: sectionID(name), Rows: in,
			Batch: slices.Contains(batchSections, name), Compliance: name == config.SectionCompliance})
	}
	return out
}

// sectionID turns a section name into a fragment: "Single sign-on (OIDC)"
// becomes "s-single-sign-on-oidc".
func sectionID(name string) string {
	var b strings.Builder
	b.WriteString("s-")
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 2 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
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

// handleSettingsSection saves a batch section as one change, all or nothing.
// Every editable row posts "v.<key>"; a secret row also posts "s.<key>", and
// its empty field keeps the stored secret unless "r.<key>" (remove) is
// checked. An empty field of any other row returns it to its default.
func handleSettingsSection(w http.ResponseWriter, r *http.Request, c OpCaller) {
	changes, expect, ok := sectionChanges(w, r)
	if !ok {
		return
	}
	args := ops.SettingsSetArgs{WriteArgs: newWriteArgs(), Changes: changes, Expect: expect}
	writeOp(w, r, c, ops.OpSettingsSet, args, "The section "+r.PostForm.Get("section")+" is saved.")
}

// handleComplianceSection saves the compliance section with compliance_set.
// The Hub requires a sign-in that is at most five minutes old; when it is
// older, the answer says where to confirm it, and the form keeps its fields.
func handleComplianceSection(w http.ResponseWriter, r *http.Request, c OpCaller) {
	changes, expect, ok := sectionChanges(w, r)
	if !ok {
		return
	}
	var res ops.ComplianceSetResult
	if err := c.CallOp(r, ops.OpComplianceSet, ops.ComplianceSetArgs{Changes: changes, Expect: expect}, &res); err != nil {
		var oe *OpError
		if errors.As(err, &oe) && oe.Code == "reauth_required" {
			writeResultError(w, "Compliance settings need a sign-in from the last five minutes. Open Hub identity (/admin/identity), confirm your sign-in there, then save this section again.")
			return
		}
		writeResultError(w, failureText(err))
		return
	}
	AddFlash(w, r, FlashInfo, "The section Compliance monitoring is saved (policy version "+strconv.FormatInt(res.Policy.Version, 10)+").")
	for _, wn := range res.Warnings {
		AddFlash(w, r, FlashWarning, wn.Message)
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// sectionChanges reads the changes of a batch section form. It writes the
// error answer and returns false when there is nothing to save.
func sectionChanges(w http.ResponseWriter, r *http.Request) ([]ops.SettingChange, []ops.SettingRevision, bool) {
	if err := r.ParseForm(); err != nil {
		writeResultError(w, "The form could not be read.")
		return nil, nil, false
	}
	var changes []ops.SettingChange
	var expect []ops.SettingRevision
	keys := make([]string, 0, len(r.PostForm))
	for k := range r.PostForm {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		key, ok := strings.CutPrefix(k, "v.")
		if !ok {
			continue
		}
		value := strings.ReplaceAll(r.PostForm.Get(k), "\r\n", "\n")
		secret := r.PostForm.Get("s."+key) == "1"
		remove := r.PostForm.Get("r."+key) == "1"
		rev := r.PostForm.Get("rev." + key)
		switch {
		case secret && remove:
			changes = append(changes, ops.SettingChange{Key: key, Unset: true, Revision: rev})
		case secret && strings.TrimSpace(value) == "":
			// Keep the stored secret, but only if it is still the one this
			// page saw: a credential rotated meanwhile must not go to the
			// endpoint saved from this page.
			if rev != "" {
				expect = append(expect, ops.SettingRevision{Key: key, Revision: rev})
			}
		case value == "":
			changes = append(changes, ops.SettingChange{Key: key, Unset: true, Revision: rev})
		default:
			changes = append(changes, ops.SettingChange{Key: key, Value: value, Revision: rev})
		}
	}
	if len(changes) == 0 {
		writeResultError(w, "Nothing to save.")
		return nil, nil, false
	}
	return changes, expect, true
}

func init() {
	RegisterRoute(Route{Pattern: "POST " + pathAdminSettingsSection, Handler: adminRoute(handleSettingsSection)})
	RegisterRoute(Route{Pattern: "POST " + pathAdminComplianceSection, Handler: adminRoute(handleComplianceSection)})
	RegisterPage(Page{Pattern: pathAdminSettings, Title: "Hub settings", Template: "settings.html", Data: adminPageData(settingsData)})
	RegisterNav(NavItem{Label: "Hub settings", Path: pathAdminSettings, Order: adminNavOrder + 10, Roles: []authz.Role{authz.RoleAdmin}, Group: NavGroupAdmin, Icon: "i-settings"})
	RegisterRoute(Route{Pattern: "POST " + pathAdminSettings + "/{key}", Handler: adminRoute(handleSettingSet)})
}
