package web

// Onboarding pages, part 1: shared plumbing and the invitations page (Story
// 6.7, R91, UX-DR7, AD-7, AD-19). The invitation keys, API keys and "Add
// worker" pages are for every signed-in user, not only for admins. Like the
// admin pages they never touch a store: every read and every change is one
// operation of the catalog, called over the REST pipeline as the signed-in user
// (AD-18), so authorisation, act-as rules and the audit trail are those of the
// API. The secrets these pages show (an invitation key, an API key) travel only
// in the answer to the post that created them; no page or list renders them
// again.
//
// The first half of this file is the plumbing the three pages share (the
// signed-in user, form routes, time and address helpers, the copy-button
// script); the second half is the invitations page itself. A user creates an
// invitation key for enrolling Workers; the answer shows the key once, with the
// ready one-line command, a file-based variant and a Compose snippet that reads
// the key from a secret file. The list shows uses, expiry and a revoke button
// and never the key.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Paths of the onboarding pages and their script.
const (
	pathInvitations = "/invitations"
	pathAPIKeys     = "/api-keys"
	pathAddWorker   = "/workers/add"
	pathOnboardingJ = "/onboarding.js"
)

// Navigation order of the onboarding entries: before the admin entries.
const onboardingNavOrder = 600

// maxOnboardingFormBytes bounds the body of an onboarding form post.
const maxOnboardingFormBytes = 16 << 10

// pageUser is the signed-in user as the onboarding pages need them.
type pageUser struct {
	// ID is the user the operations run for: the target while an admin acts as
	// another user, else the signed-in user.
	ID       string
	Username string
	// Admin is true for an admin acting as themselves. While acting as another
	// user the API treats the session as that user, so the admin-only choices
	// (infrastructure keys, "everything", manage) are not offered.
	Admin bool
}

func userOf(r *http.Request) (pageUser, bool) {
	sess, ok := session.FromContext(r.Context())
	if !ok {
		return pageUser{}, false
	}
	u := pageUser{ID: sess.UserID, Username: sess.Username, Admin: sess.Role == authz.RoleAdmin && sess.ActAs == nil}
	if sess.ActAs != nil {
		u.ID, u.Username = sess.ActAs.TargetID, sess.ActAs.TargetUsername
	}
	return u, true
}

// userPageData runs fn as the data function of a page for any signed-in user:
// the connected caller, and the mapping of a failed call to a page status.
func userPageData(fn func(r *http.Request, c OpCaller, u pageUser) (any, error)) func(r *http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		u, ok := userOf(r)
		if !ok {
			return nil, ErrNotFound()
		}
		c, err := adminCaller()
		if err != nil {
			return nil, err
		}
		data, err := fn(r, c, u)
		if err != nil {
			return nil, pageError(err)
		}
		return data, nil
	}
}

// userRoute wraps the handler of an onboarding form post: a connected caller
// and a bounded form body. The shell has already required a session.
func userRoute(fn func(w http.ResponseWriter, r *http.Request, c OpCaller, u pageUser)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := userOf(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		c, err := adminCaller()
		if err != nil {
			writeResultError(w, "This page is not connected to the Hub yet.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxOnboardingFormBytes)
		if err := r.ParseForm(); err != nil {
			writeResultError(w, "The form could not be read.")
			return
		}
		fn(w, r, c, u)
	})
}

// onboardingNow is the clock of the pages; tests replace it.
var onboardingNow = time.Now

// formatTime renders an optional timestamp for a table cell; none is the text
// for an absent one.
func formatTime(t *ops.Timestamp, none string) string {
	if t == nil {
		return none
	}
	return t.Time().Format("2006-01-02 15:04") + " UTC"
}

func isoTime(t *ops.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.Time().Format(time.RFC3339)
}

// Choice is one entry of a select or a group of checkboxes.
type Choice struct {
	Value    string
	Label    string
	Help     string
	Selected bool
}

// -- the Hub address in the Worker commands ---------------------------------

// publicURL is the optional source of the Hub's configured public URL
// (MINION_PUBLIC_URL). Without it the address comes from the request.
var publicURL atomic.Pointer[func(*http.Request) string]

// SetPublicURL tells the onboarding pages how to find the Hub's public URL
// (the Hub's web module passes the configured MINION_PUBLIC_URL). An empty
// answer, or no source, makes the pages use the address of the request; nil
// removes the source. The returned function removes this source again, but only
// while it is still the current one, so a Hub that stops late never removes the
// source of a Hub that started after it in the same process.
func SetPublicURL(f func(r *http.Request) string) (release func()) {
	if f == nil {
		publicURL.Store(nil)
		return func() {}
	}
	p := &f
	publicURL.Store(p)
	return func() { publicURL.CompareAndSwap(p, nil) }
}

// placeholderHub is shown when no usable address is known.
const placeholderHub = "https://YOUR-HUB-ADDRESS"

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)

// hubURL returns the address that Workers use to reach this Hub, without a
// trailing slash: the configured public URL, else the scheme and host of the
// request. The last X-Forwarded-Proto value is honoured unconditionally, from
// any client: it can only change the scheme shown on the requester's own page,
// and the configured public URL always takes precedence over it.
func hubURL(r *http.Request) string {
	if f := publicURL.Load(); f != nil {
		if u := strings.TrimRight((*f)(r), "/"); u != "" {
			return u
		}
	}
	if !hostPattern.MatchString(r.Host) {
		return placeholderHub
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	vals := strings.Split(strings.Join(r.Header.Values("X-Forwarded-Proto"), ","), ",")
	switch strings.ToLower(strings.TrimSpace(vals[len(vals)-1])) {
	case "https":
		scheme = "https"
	case "http":
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// shellArg quotes s for a POSIX shell when it needs it.
func shellArg(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// yamlString quotes s as a double-quoted YAML scalar (a JSON string is one).
func yamlString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSpace(buf.String())
}

// -- the copy button ---------------------------------------------------------

// onboardingScript is the only script of the onboarding pages: a delegated
// handler for buttons with data-copy="#id", which copy the text or value of
// that element. The Content-Security-Policy allows scripts only from this
// origin, so it is served by a route instead of being inline.
const onboardingScript = `(function () {
  'use strict';
  function textOf(el) { return el.value !== undefined ? el.value : el.textContent; }
  function selectAll(el) {
    if (typeof el.select === 'function') { el.select(); return; }
    var range = document.createRange();
    range.selectNodeContents(el);
    var sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  }
  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest ? ev.target.closest('[data-copy]') : null;
    if (!btn) { return; }
    var el = document.querySelector(btn.getAttribute('data-copy'));
    if (!el) { return; }
    var label = btn.getAttribute('data-label') || btn.textContent;
    btn.setAttribute('data-label', label);
    selectAll(el);
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(textOf(el)).then(
        function () { btn.textContent = 'Copied'; setTimeout(function () { btn.textContent = label; }, 2000); },
        function () { btn.textContent = 'Press Ctrl+C to copy'; });
    } else {
      btn.textContent = 'Press Ctrl+C to copy';
    }
  });
})();
`

func handleOnboardingScript(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(onboardingScript))
}

func init() {
	RegisterRoute(Route{Pattern: "GET " + pathOnboardingJ, Handler: http.HandlerFunc(handleOnboardingScript)})
}

// -- the invitations page ---------------------------------------------------

// InvitationsPage is what invitations.html sees as .Data.
type InvitationsPage struct {
	Invitations []InvitationRow
	Modes       []Choice
	TTLs        []Choice
	// Admin offers the key kind (infrastructure) and the "everybody's keys"
	// switch.
	Admin bool
	// All is true while the list shows the keys of every user.
	All bool
	// MaxUsesDefault and MaxUsesLimit are the use limit offered for a reusable
	// key; MaxUsesLimit is 0 for an admin (no upper limit besides the Hub's).
	MaxUsesDefault int
	MaxUsesLimit   int
	// Notices are the texts the upgrade left on keys it limited (F-02), so the
	// owner learns why a key now expires or runs out.
	Notices []InvitationNotice
}

// InvitationNotice is the upgrade notice of one key.
type InvitationNotice struct {
	Name string
	Text string
}

// InvitationRow is one invitation key as the list shows it. It has no secret.
type InvitationRow struct {
	ID        string
	Name      string
	Mode      string
	Kind      string
	Uses      string
	Expires   string
	ExpiresAt string
	Status    string
	CreatorID string
	// CanRevoke is true for a key that can still enroll Workers.
	CanRevoke bool
	// Notice is the text the upgrade to 0.6 left on the key.
	Notice string
}

var invitationModes = []Choice{
	{Value: ops.InvitationOneTime, Label: "One-time (enrolls one Worker)", Selected: true},
	{Value: ops.InvitationReusable, Label: "Reusable (enrolls any number of Workers until it expires)"},
}

var invitationTTLLabels = map[string]string{
	ops.TTL1h: "1 hour", ops.TTL4h: "4 hours", ops.TTL8h: "8 hours", ops.TTL24h: "24 hours",
	ops.TTL1w: "1 week", ops.TTL1m: "1 month", ops.TTLUnlimited: "Unlimited",
}

// invitationTTLChoices lists the TTLs a person may choose; 24 hours is
// preselected. Only an admin may choose a key that is valid longer than a week.
func invitationTTLChoices(admin bool) []Choice {
	var out []Choice
	max, _ := ops.InvitationTTLDuration(ops.NonAdminMaxInvitationTTL)
	for _, t := range ops.InvitationTTLs() {
		if d, _ := ops.InvitationTTLDuration(t); !admin && (d == 0 || d > max) {
			continue
		}
		out = append(out, Choice{Value: t, Label: invitationTTLLabels[t], Selected: t == ops.TTL24h})
	}
	return out
}

func invitationRow(i ops.InvitationInfo) InvitationRow {
	row := InvitationRow{
		ID: i.ID, Name: i.Name, Kind: i.Kind, Status: i.Status, CreatorID: i.CreatorID,
		Expires: formatTime(i.ExpiresAt, "never"), ExpiresAt: isoTime(i.ExpiresAt),
		CanRevoke: i.Status == ops.InvitationActive, Notice: i.Notice,
	}
	if row.Name == "" {
		row.Name = "(no name)"
	}
	if i.Source == "infra_file" {
		row.Name += " (MINION_INFRA_INVITE_KEY_FILE)"
		row.CreatorID = "the key file"
	}
	row.Mode = "one-time"
	if i.Mode == ops.InvitationReusable {
		row.Mode = "reusable"
	}
	if i.MaxUses != nil {
		row.Uses = fmt.Sprintf("%d of %d", i.Uses, *i.MaxUses)
	} else {
		row.Uses = fmt.Sprintf("%d", i.Uses)
	}
	return row
}

func invitationsData(r *http.Request, c OpCaller, u pageUser) (any, error) {
	all := u.Admin && r.URL.Query().Get("all") == "1"
	var res ops.InvitationListResult
	if err := c.CallOp(r, ops.OpInvitationList, ops.InvitationListArgs{All: all}, &res); err != nil {
		return nil, err
	}
	page := InvitationsPage{Modes: invitationModes, TTLs: invitationTTLChoices(u.Admin), Admin: u.Admin, All: all,
		MaxUsesDefault: ops.DefaultReusableMaxUses}
	if !u.Admin {
		page.MaxUsesLimit = ops.MaxReusableUsesNonAdmin
	}
	for _, i := range res.Invitations {
		row := invitationRow(i)
		page.Invitations = append(page.Invitations, row)
		if row.Notice != "" && i.Status == ops.InvitationActive {
			page.Notices = append(page.Notices, InvitationNotice{Name: row.Name, Text: row.Notice})
		}
	}
	return page, nil
}

// InviteCommands are the texts the page offers for one new key. They hold the
// key itself, so they exist only in the answer to the post that created it.
type InviteCommands struct {
	// OneLiner is the ready command (R85): the key is a literal argument.
	OneLiner string
	// FileVariant keeps the key out of the process list: a subshell creates a
	// private directory, writes the key into a new file there with a
	// here-document (a shell built-in), runs the command on that file and
	// removes the directory again.
	FileVariant string
	// Compose is a complete Compose project that runs the Worker from an
	// ordinary base image with bootstrap.sh and reads the key from a secret
	// file.
	Compose string
}

// workerContainerUID is the user the Compose Worker runs as. It is not root and
// has no meaning on the host; the init service hands it the data and the key.
const workerContainerUID = 10001

// fileVariant renders the "key in a file" commands.
//
// The key file is never written to a name the caller or anyone else could have
// prepared: mktemp -d creates a new directory with a random name and mode 0700
// (the umask 077 is a second guard), and the key goes into a file created inside
// it, so an existing file of any mode and a symbolic link at a guessable path
// can neither be overwritten nor redirect the secret. Every step is chained
// with &&, so a failed step stops the chain and the Worker command never runs
// without its key file; the EXIT trap removes the directory on every way out
// and the exit status is the one of the Worker command. It all runs in a
// subshell, so it leaves the umask and the traps of the user's shell alone.
func fileVariant(srv, key string) string {
	var f strings.Builder
	f.WriteString("(\n")
	f.WriteString("  umask 077 &&\n")
	f.WriteString("  dir=$(mktemp -d \"${TMPDIR:-/tmp}/minion-invite.XXXXXX\") &&\n")
	f.WriteString("  trap 'rm -rf \"$dir\"' EXIT &&\n")
	f.WriteString("  cat > \"$dir/invite.key\" <<'EOF' &&\n")
	f.WriteString(key + "\nEOF\n")
	f.WriteString("  minion worker up --server " + srv + " --invite-key \"$dir/invite.key\"\n")
	f.WriteString(")\n")
	return f.String()
}

// inviteCommands renders the three command texts for key and the Hub address.
func inviteCommands(server, key string) InviteCommands {
	srv := shellArg(server)
	return InviteCommands{
		OneLiner:    "minion worker up --server " + srv + " --invite-key " + key,
		FileVariant: fileVariant(srv, key),
		Compose:     composeFile(server),
	}
}

// composeFile renders the Compose project of a Worker. It holds no secret: the
// key is the content of invite_key.txt, which the user creates.
//
// The Worker needs the minion binary and tmux. The project uses an ordinary
// Alpine image with tmux added, and deploy/bootstrap.sh (a release asset, see
// deploy/compose/hub-bootstrap.yaml) as the entrypoint: it fetches, verifies and
// caches the binary and then runs `minion "$@"`, so the command is the
// arguments of minion only, "worker up", never "minion worker up" (the binary
// name twice). Nothing runs as root but the one-shot init service, which gives
// the Worker's user the state directory, the download cache and the key file.
func composeFile(server string) string {
	uid := fmt.Sprintf("%d:%d", workerContainerUID, workerContainerUID)
	var c strings.Builder
	c.WriteString("# A Minion Worker in Docker, started from the invitation key. Before you start:\n")
	c.WriteString("# 1. Put bootstrap.sh next to this file. It is a file of the Minion release:\n")
	c.WriteString("#    https://github.com/patrick-1984/ai-minion/releases/latest/download/bootstrap.sh\n")
	c.WriteString("# 2. Save the key as the only line of invite_key.txt next to this file.\n")
	c.WriteString("# 3. Run: docker compose up -d\n")
	c.WriteString("# 4. Approve the new Worker in the Hub (Workers page): it starts pending and gets no operations until you do.\n")
	c.WriteString("# The image is built from Alpine with tmux (the Worker drives its sessions through tmux);\n")
	c.WriteString("# bootstrap.sh downloads and verifies the minion binary on the first start and keeps it in\n")
	c.WriteString("# the cache volume. The Worker runs as user " + uid + " and enrolls once: after that the key\n")
	c.WriteString("# is not needed, but KEEP the file invite_key.txt: Docker needs the mounted file to exist\n")
	c.WriteString("# whenever the container is restarted or recreated, and the Worker reads it on every start.\n")
	c.WriteString("# An empty file is safe (an enrolled Worker ignores the key), a missing one stops the start.\n")
	c.WriteString("# After enrollment erase the secret in place. The init service made the file belong to user\n")
	c.WriteString("# " + uid + " (mode 600), so your own shell may not write to it; let the root init service do it:\n")
	c.WriteString("#    docker compose run --rm --no-deps init sh -c ': > /invite_key.txt'\n")
	c.WriteString("# That truncates the same file (do not delete, move or replace it with an editor). To remove\n")
	c.WriteString("# the file itself instead, first remove invite_key.txt from init's volumes, the secret and\n")
	c.WriteString("# MINION_INVITE_KEY_FILE, then run: docker compose up -d --remove-orphans\n")
	c.WriteString("services:\n")
	c.WriteString("  init:\n")
	c.WriteString("    image: alpine:3.22\n")
	c.WriteString("    user: \"0:0\"\n")
	c.WriteString("    command: [\"sh\", \"-c\", \"chown -R " + uid + " /data /cache && chown " + uid + " /invite_key.txt && chmod 600 /invite_key.txt\"]\n")
	c.WriteString("    volumes:\n")
	c.WriteString("      - ./worker-data:/data\n")
	c.WriteString("      - minion-cache:/cache\n")
	c.WriteString("      - ./invite_key.txt:/invite_key.txt\n")
	c.WriteString("  minion-worker:\n")
	c.WriteString("    build:\n")
	c.WriteString("      context: .\n")
	c.WriteString("      dockerfile_inline: |\n")
	c.WriteString("        FROM alpine:3.22\n")
	c.WriteString("        RUN apk add --no-cache tmux\n")
	c.WriteString("    user: \"" + uid + "\"\n")
	c.WriteString("    depends_on:\n")
	c.WriteString("      init:\n")
	c.WriteString("        condition: service_completed_successfully\n")
	c.WriteString("    restart: unless-stopped\n")
	c.WriteString("    entrypoint: [\"/bin/sh\", \"/bootstrap.sh\"]\n")
	c.WriteString("    command: [\"worker\", \"up\"]\n")
	c.WriteString("    environment:\n")
	c.WriteString("      MINION_SERVER: " + yamlString(strings.ReplaceAll(server, "$", "$$")) + "\n")
	c.WriteString("      MINION_VERSION: ${MINION_VERSION:-latest}\n")
	c.WriteString("      MINION_CACHE_DIR: /cache\n")
	c.WriteString("      MINION_STATE_DIR: /data\n")
	c.WriteString("      MINION_INVITE_KEY_FILE: /run/secrets/minion_invite_key\n")
	c.WriteString("    volumes:\n")
	c.WriteString("      - ./bootstrap.sh:/bootstrap.sh:ro\n")
	c.WriteString("      - ./worker-data:/data\n")
	c.WriteString("      - minion-cache:/cache\n")
	c.WriteString("    secrets:\n")
	c.WriteString("      - minion_invite_key\n")
	c.WriteString("secrets:\n")
	c.WriteString("  minion_invite_key:\n")
	c.WriteString("    file: ./invite_key.txt\n")
	c.WriteString("volumes:\n")
	c.WriteString("  minion-cache:\n")
	return c.String()
}

// inviteResult is what the invitation_key fragment renders: the new key, once.
type inviteResult struct {
	Name    string
	Mode    string
	Kind    string
	Expires string
	// Limit is "N enrollments" for a key with a use limit.
	Limit string
	Key   string
	InviteCommands
}

var invitationFragments = fragments("invitations.html")

func handleInvitationCreate(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.InvitationCreateArgs{
		Mode: r.PostFormValue("mode"),
		TTL:  r.PostFormValue("ttl"),
		Name: strings.TrimSpace(r.PostFormValue("name")),
		Kind: r.PostFormValue("kind"),
	}
	if v := strings.TrimSpace(r.PostFormValue("max_uses")); v != "" && args.Mode == ops.InvitationReusable {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeResultError(w, "The number of uses must be a whole number.")
			return
		}
		args.MaxUses = n
	}
	var res ops.InvitationCreateResult
	if err := c.CallOp(r, ops.OpInvitationCreate, args, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	out := inviteResult{
		Name: res.Name, Mode: "one-time", Kind: res.Kind, Expires: formatTime(res.ExpiresAt, "never"),
		Key: res.Key, InviteCommands: inviteCommands(hubURL(r), res.Key),
	}
	if res.Mode == ops.InvitationReusable {
		out.Mode = "reusable"
	}
	if res.MaxUses != nil {
		out.Limit = fmt.Sprintf("%d %s", *res.MaxUses, map[bool]string{true: "enrollment", false: "enrollments"}[*res.MaxUses == 1])
	}
	writeFragmentTemplate(w, invitationFragments(), "invitation_key", out)
}

func handleInvitationRevoke(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	args := ops.InvitationRevokeArgs{WriteArgs: newWriteArgs(), ID: r.PathValue("id")}
	writeOp(w, r, c, ops.OpInvitationRevoke, args, "The invitation key is revoked. Workers that already enrolled are not affected.")
}

func init() {
	RegisterPage(Page{Pattern: pathInvitations, Title: "Invitations", Template: "invitations.html", Data: userPageData(invitationsData)})
	RegisterNav(NavItem{Label: "Invitations", Path: pathInvitations, Order: onboardingNavOrder, Group: NavGroupAccess, Icon: "i-invite"})
	RegisterRoute(Route{Pattern: "POST " + pathInvitations, Handler: userRoute(handleInvitationCreate)})
	RegisterRoute(Route{Pattern: "POST " + pathInvitations + "/{id}/revoke", Handler: userRoute(handleInvitationRevoke)})
}
