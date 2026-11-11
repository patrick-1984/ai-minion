package web

// The API keys page (Story 6.7, R44, R51, R52, AD-11). A user creates a key
// limited to resources they own (their account, one Worker, one session) and to
// chosen actions; an admin can also pick "everything" and the manage action.
// The key is shown once, in the answer to the post that created it. The list
// shows the 8-character hint, a summary of the scope, the expiry and the last
// use (the minute the key last authenticated, or "never used"), and a revoke button; it never shows a key.

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// APIKeysPage is what apikeys.html sees as .Data.
type APIKeysPage struct {
	Keys      []APIKeyRow
	Resources []Choice
	Actions   []Choice
	Expiries  []Choice
	// BoundWorkers are the Workers a key may be bound to (a call key).
	BoundWorkers []Choice
	Admin        bool
	// ResourceNote says why the Worker choices are missing, when they are.
	ResourceNote string
}

// APIKeyRow is one key as the list shows it. It has no secret.
type APIKeyRow struct {
	ID      string
	Name    string
	Hint    string
	Scope   string
	Expires string
	// ExpiresAt is the machine-readable expiry, empty when the key never expires.
	ExpiresAt string
	LastUsed  string
	State     string
	CanRevoke bool
}

// apiKeyExpiries are the lifetimes offered, in days; 0 never expires.
var apiKeyExpiries = []struct {
	Value string
	Label string
	Days  int
}{
	{"30d", "30 days", 30},
	{"90d", "90 days", 90},
	{"1y", "1 year", 365},
	{"never", "Never", 0},
}

const defaultAPIKeyExpiry = "90d"

// apiKeyExpiryChoices lists the lifetimes; "Never" is offered to admins only
// (F-07: a key without an expiry is for admins).
func apiKeyExpiryChoices(admin bool) []Choice {
	var out []Choice
	for _, e := range apiKeyExpiries {
		if e.Days == 0 && !admin {
			continue
		}
		out = append(out, Choice{Value: e.Value, Label: e.Label, Selected: e.Value == defaultAPIKeyExpiry})
	}
	return out
}

// apiKeyActionChoices lists the actions a user may give a key; manage only
// for an admin.
func apiKeyActionChoices(admin bool) []Choice {
	out := []Choice{
		{Value: string(ops.ActionRead), Label: "Read", Help: "see Workers, sessions and screens", Selected: true},
		{Value: string(ops.ActionOperate), Label: "Operate", Help: "type, send keys, create sessions, answer dialogs"},
		{Value: string(ops.ActionMessage), Label: "Message", Help: "send messages to agent sessions only"},
	}
	if admin {
		out = append(out, Choice{Value: string(ops.ActionManage), Label: "Manage", Help: "administer the Hub"})
	}
	return out
}

// Resource values of the form: "everything", "user:<id>", "worker:<id>".
const (
	resEverything = "everything"
	resUserPrefix = "user:"
	resWorkerPref = "worker:"
)

// apiKeyResourceChoices lists what the user may pick: their account, each of
// their Workers, and for an admin everything.
func apiKeyResourceChoices(u pageUser, workers []ops.WorkerInfo) []Choice {
	var out []Choice
	if u.Admin {
		out = append(out, Choice{Value: resEverything, Label: "Everything", Help: "every user, Worker and session of the Hub"})
	}
	out = append(out, Choice{Value: resUserPrefix + u.ID, Label: "My account", Help: "all my Workers and sessions, now and later", Selected: true})
	for _, w := range workers {
		if w.OwnerID != "" && w.OwnerID != u.ID {
			continue
		}
		out = append(out, Choice{Value: resWorkerPref + w.ID, Label: "Worker " + w.Name, Help: "this Worker and all its sessions"})
	}
	return out
}

// apiKeyBoundWorkerChoices lists the Workers of the user a call key can be
// bound to; the Hub checks the binding again.
func apiKeyBoundWorkerChoices(u pageUser, workers []ops.WorkerInfo) []Choice {
	var out []Choice
	for _, w := range workers {
		if w.OwnerID != "" && w.OwnerID != u.ID {
			continue
		}
		out = append(out, Choice{Value: w.ID, Label: w.Name})
	}
	return out
}

// scopeSummary describes the resources and actions of a key in one line.
func scopeSummary(k ops.APIKeyInfo, userID string, workerNames map[string]string) string {
	var res []string
	for _, r := range k.Resources {
		res = append(res, resourceLabel(r, userID, workerNames))
	}
	acts := make([]string, len(k.Actions))
	for i, a := range k.Actions {
		acts[i] = string(a)
	}
	s := strings.Join(res, ", ") + ": " + strings.Join(acts, ", ")
	if k.BoundWorkerID != "" {
		s += " (bound to Worker " + nameOrID(k.BoundWorkerID, workerNames) + ")"
	}
	return s
}

func nameOrID(id string, names map[string]string) string {
	if n := names[id]; n != "" {
		return n
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func resourceLabel(r ops.KeyResource, userID string, workerNames map[string]string) string {
	switch r.Kind {
	case ops.KeyResourceEverything:
		return "everything"
	case ops.KeyResourceUser:
		if r.UserID == userID {
			return "my account"
		}
		return "user " + nameOrID(r.UserID, nil)
	case ops.KeyResourceWorker:
		return "Worker " + nameOrID(r.WorkerID, workerNames)
	case ops.KeyResourceSession:
		return "session " + r.SessionName + " on " + nameOrID(r.WorkerID, workerNames)
	}
	return r.Kind
}

func apiKeyRow(k ops.APIKeyInfo, userID string, workerNames map[string]string, now time.Time) APIKeyRow {
	row := APIKeyRow{
		ID: k.ID, Name: k.Name, Hint: "mn_" + k.Hint + "...", Scope: scopeSummary(k, userID, workerNames),
		Expires: formatTime(k.ExpiresAt, "never"), ExpiresAt: isoTime(k.ExpiresAt),
		LastUsed: formatTime(k.LastUsedAt, "never used"), State: "active",
	}
	switch {
	case k.RevokedAt != nil:
		row.State = "revoked"
	case k.ExpiresAt != nil && !k.ExpiresAt.Time().After(now):
		row.State = "expired"
	}
	row.CanRevoke = k.RevokedAt == nil
	return row
}

func apiKeysData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	var keys ops.APIKeyListResult
	if err := c.CallOp(r, ops.OpAPIKeyList, ops.APIKeyListArgs{}, &keys); err != nil {
		return nil, err
	}
	// The Worker names serve the choices and the scope summaries. Without
	// them the page still works with the account choice.
	workers, names, ok := loadWorkerNames(r, c)
	note := ""
	if !ok {
		note = "Your Workers could not be loaded, so only the account choice is offered."
	}
	page := APIKeysPage{
		Resources: apiKeyResourceChoices(u, workers.Workers), Actions: apiKeyActionChoices(u.Admin),
		Expiries: apiKeyExpiryChoices(u.Admin), Admin: u.Admin, ResourceNote: note,
		BoundWorkers: apiKeyBoundWorkerChoices(u, workers.Workers),
	}
	now := onboardingNow()
	for _, k := range keys.Keys {
		page.Keys = append(page.Keys, apiKeyRow(k, u.ID, names, now))
	}
	return page, nil
}

// loadWorkerNames lists the Workers of the user and maps their ids to names;
// ok is false when the Hub could not list them.
func loadWorkerNames(r *http.Request, c OpCaller) (ops.WorkersListResult, map[string]string, bool) {
	var workers ops.WorkersListResult
	names := map[string]string{}
	if err := c.CallOp(r, ops.OpWorkersList, ops.WorkersListArgs{}, &workers); err != nil {
		return ops.WorkersListResult{}, names, false
	}
	for _, w := range workers.Workers {
		names[w.ID] = w.Name
	}
	return workers, names, true
}

// errKeyForm is a mistake in the create form; its text is shown to the user.
type errKeyForm string

func (e errKeyForm) Error() string { return string(e) }

// buildAPIKeyArgs turns the create form into the arguments of apikey_create.
// The Hub decides again what the user may grant; the checks here only give
// plain answers for the common mistakes before a round trip.
//
// Form fields: name; resource (repeated); action (repeated); session (optional,
// needs exactly one Worker); bound_worker (optional, a call key for that
// Worker); expires.
func buildAPIKeyArgs(form map[string][]string, u pageUser, now time.Time) (ops.APIKeyCreateArgs, error) {
	first := func(k string) string {
		if v := form[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	args := ops.APIKeyCreateArgs{Name: first("name")}
	if args.Name == "" {
		return args, errKeyForm("Give the key a name so you can recognise it in the list.")
	}
	if len(args.Name) > ops.MaxAPIKeyNameLen {
		return args, errKeyForm(fmt.Sprintf("The name can have at most %d characters.", ops.MaxAPIKeyNameLen))
	}
	args.BoundWorkerID = first("bound_worker")
	session := first("session")
	workers := 0
	for _, v := range dedupe(form["resource"]) {
		switch {
		case v == resEverything:
			if !u.Admin {
				return args, errKeyForm("Only an admin can create a key for everything.")
			}
			args.Resources = append(args.Resources, ops.KeyResource{Kind: ops.KeyResourceEverything})
		case strings.HasPrefix(v, resUserPrefix):
			args.Resources = append(args.Resources, ops.KeyResource{Kind: ops.KeyResourceUser, UserID: strings.TrimPrefix(v, resUserPrefix)})
		case strings.HasPrefix(v, resWorkerPref):
			workers++
			args.Resources = append(args.Resources, ops.KeyResource{Kind: ops.KeyResourceWorker, WorkerID: strings.TrimPrefix(v, resWorkerPref)})
		default:
			return args, errKeyForm("Choose what the key may reach from the list.")
		}
	}
	switch {
	case len(args.Resources) == 0:
		return args, errKeyForm("Choose at least one thing the key may reach.")
	case len(args.Resources) > 1 && hasEverything(args.Resources):
		return args, errKeyForm("Everything cannot be combined with other choices.")
	case session != "" && (workers != 1 || len(args.Resources) != 1):
		return args, errKeyForm("To limit a key to one session, choose exactly one Worker and nothing else.")
	}
	if session != "" {
		args.Resources = []ops.KeyResource{{Kind: ops.KeyResourceSession, WorkerID: args.Resources[0].WorkerID, SessionName: session}}
	}
	for _, a := range dedupe(form["action"]) {
		act := ops.Action(a)
		if !act.Valid() {
			return args, errKeyForm("Choose what the key may do from the list.")
		}
		if act == ops.ActionManage && !u.Admin {
			return args, errKeyForm("Only an admin can give a key the manage action.")
		}
		args.Actions = append(args.Actions, act)
	}
	if slices.Contains(args.Actions, ops.ActionManage) && !hasEverything(args.Resources) {
		return args, errKeyForm("The manage action is available only for a key that reaches everything.")
	}
	if len(args.Actions) == 0 {
		return args, errKeyForm("Choose at least one thing the key may do.")
	}
	exp := first("expires")
	if exp == "" {
		exp = defaultAPIKeyExpiry
	}
	found := false
	for _, e := range apiKeyExpiries {
		if e.Value != exp {
			continue
		}
		found = true
		if e.Days == 0 {
			if !u.Admin {
				return args, errKeyForm("Only an admin can create a key that never expires.")
			}
			args.Never = true
		}
		if e.Days > 0 {
			ts := ops.Timestamp(now.UTC().Add(time.Duration(e.Days) * 24 * time.Hour).Truncate(time.Millisecond))
			args.ExpiresAt = &ts
		}
	}
	if !found {
		return args, errKeyForm("Choose how long the key stays valid from the list.")
	}
	return args, nil
}

func hasEverything(rs []ops.KeyResource) bool {
	for _, r := range rs {
		if r.Kind == ops.KeyResourceEverything {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// apiKeyResult is what the apikey_secret fragment renders: the new key, once.
type apiKeyResult struct {
	Name    string
	Key     string
	Hint    string
	Scope   string
	Expires string
	Server  string
}

var apiKeyFragments = fragments("apikeys.html")

func handleAPIKeyCreate(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser) {
	args, err := buildAPIKeyArgs(r.PostForm, u, onboardingNow())
	if err != nil {
		writeResultError(w, err.Error())
		return
	}
	var res ops.APIKeyCreateResult
	if err := c.CallOp(r, ops.OpAPIKeyCreate, args, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	_, names, _ := loadWorkerNames(r, c)
	writeFragmentTemplate(w, apiKeyFragments(), "apikey_secret", apiKeyResult{
		Name: res.Info.Name, Key: res.Key, Hint: "mn_" + res.Info.Hint + "...",
		Scope: scopeSummary(res.Info, u.ID, names), Expires: formatTime(res.Info.ExpiresAt, "never"), Server: hubURL(r),
	})
}

func handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.APIKeyRevokeArgs{WriteArgs: newWriteArgs(), ID: r.PathValue("id")}
	writeOp(w, r, c, ops.OpAPIKeyRevoke, args, "The API key is revoked.")
}

func init() {
	RegisterPage(Page{Pattern: pathAPIKeys, Title: "API keys", Template: "apikeys.html", Data: userPageData(apiKeysData)})
	RegisterNav(NavItem{Label: "API keys", Path: pathAPIKeys, Order: onboardingNavOrder + 10, Group: NavGroupAccess, Icon: "i-key"})
	RegisterRoute(Route{Pattern: "POST " + pathAPIKeys, Handler: userRoute(handleAPIKeyCreate)})
	RegisterRoute(Route{Pattern: "POST " + pathAPIKeys + "/{id}/revoke", Handler: userRoute(handleAPIKeyRevoke)})
}
