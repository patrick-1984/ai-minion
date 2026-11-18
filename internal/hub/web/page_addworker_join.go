package web

// "Add a Worker in one step" on the Add worker page (v0.12.0). The user picks
// how long the offer is valid (and optionally a name); the post creates a
// ONE-USE invitation key through the invitation_create operation, as the
// signed-in user, and answers once with a join token (identity.JoinToken:
// mnj1-..., the Hub URL plus the key, whose fingerprint is the full Hub
// fingerprint) and the ready commands for every platform. The blueprints are
// the Docker Swarm join token, kubeadm join with
// --discovery-token-ca-cert-hash, the edge agent of Arcane (one docker run
// with environment variables) and the one-off setup keys of Tailscale and
// NetBird. Like every onboarding secret, the token travels only in the answer
// to the post: it is never logged, stored by the page or rendered again.

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// pathAddWorkerJoin is the post that creates the one-step join command.
const pathAddWorkerJoin = pathAddWorker + "/join"

// joinTTLChoices lists the validity choices of the one-step form: those of the
// invitations page, with 1 hour preselected (a join command is used right
// away).
func joinTTLChoices(admin bool) []Choice {
	out := invitationTTLChoices(admin)
	for i := range out {
		out[i].Selected = out[i].Value == ops.TTL1h
	}
	return out
}

// JoinCommands are the texts the page offers for one join token. They hold the
// secret, so they exist only in the answer to the post that created it.
type JoinCommands struct {
	// Unix is `minion worker up --join <token>` for Linux and macOS without
	// sudo: a user service, and the sharing question in a terminal.
	Unix string
	// System installs the Linux system service with sudo: the token goes
	// through a private file (MINION_JOIN_FILE), never into sudo's argv or
	// log, and the sharing choice is given, because root cannot ask.
	System string
	// Windows is the PowerShell command of the Windows preview.
	Windows string
	// Expanded is the same with --server, --hub-fingerprint and --invite-key,
	// for Workers before 0.12.0 and for scripts.
	Expanded string
	// Docker is one docker run, after the download of bootstrap.sh.
	Docker string
	// Compose is a complete Compose project with MINION_JOIN.
	Compose string
}

// hubVersion is the version of this Hub; tests replace it.
var hubVersion = func() string { return version.Version }

// workerVersion is the minion release a container Worker runs: the Hub's own
// release (a join token needs a Worker that knows it, and the latest stable
// release may be older than a release candidate Hub), or "latest" for a
// development build.
func workerVersion() string {
	v, err := version.ParseVersion(hubVersion())
	if err != nil {
		return "latest"
	}
	return v.String()
}

// joinCommands renders the commands for tok; name is the optional Worker name.
func joinCommands(tok identity.JoinToken, name string) JoinCommands {
	text := tok.Encode()
	var unixName, psName string
	if name != "" {
		unixName = " --name " + shellArg(name)
		psName = " --name " + psArg(name)
	}
	return JoinCommands{
		Unix:    "minion worker up" + unixName + " --join " + shellArg(text),
		System:  systemJoin(text, unixName),
		Windows: "minion.exe worker up --foreground" + psName + " --join " + psArg(text),
		Expanded: "minion worker up --server " + shellArg(tok.HubURL) + " --hub-fingerprint " + tok.Fingerprint().String() +
			unixName + " --invite-key " + tok.Invite.String(),
		Docker:  dockerRunJoin(text, name),
		Compose: joinComposeFile(text, name),
	}
}

// systemJoin renders the sudo variant. Like the invitation's file variant,
// the token goes into a new file in a fresh private directory that the EXIT
// trap removes; `sudo env` passes only the path, so the token is neither in
// argv nor in sudo's log. Root cannot ask the sharing question for the user,
// so the command gives --share-existing later (nothing is shared until the
// user chooses in the Hub).
func systemJoin(token, nameArg string) string {
	var f strings.Builder
	f.WriteString("(\n")
	f.WriteString("  umask 077 &&\n")
	f.WriteString("  dir=$(mktemp -d \"${TMPDIR:-/tmp}/minion-join.XXXXXX\") &&\n")
	f.WriteString("  trap 'rm -rf \"$dir\"' EXIT &&\n")
	f.WriteString("  cat > \"$dir/join\" <<'EOF' &&\n")
	f.WriteString(token + "\nEOF\n")
	f.WriteString("  sudo env MINION_JOIN_FILE=\"$dir/join\" minion worker up --share-existing later" + nameArg + "\n")
	f.WriteString(")\n")
	return f.String()
}

// psQuotes are the characters PowerShell treats as a single quote: the ASCII
// apostrophe and the typographic single quotes.
var psQuotes = []string{"'", "\u2018", "\u2019", "\u201a", "\u201b"}

// psArg quotes s for PowerShell when it needs it: a single-quoted string, in
// which only the single quotes are special; each one is doubled.
func psArg(s string) string {
	if shellSafe.MatchString(s) && !strings.ContainsAny(s, "@%+=,") {
		return s
	}
	for _, q := range psQuotes {
		s = strings.ReplaceAll(s, q, q+q)
	}
	return "'" + s + "'"
}

// dockerRunJoin renders the docker run command of a Worker with MINION_JOIN.
// The Worker runs in an Alpine container with tmux; bootstrap.sh downloads,
// verifies and caches the minion binary and runs it. Every step is chained
// with &&: docker runs only after a complete download, which replaces any
// older bootstrap.sh, and --mount fails if the file is missing (a -v would
// create a directory). MINION_VERSION is the Hub's release. The token stays
// in the container's environment: on every start the Worker takes the Hub
// address and fingerprint from it, and an enrolled Worker ignores the used
// key.
func dockerRunJoin(token, name string) string {
	var d strings.Builder
	d.WriteString("curl -fsSL -o bootstrap.sh.new " + bootstrapURL + " &&\n")
	d.WriteString("mv -f bootstrap.sh.new bootstrap.sh &&\n")
	d.WriteString("docker run -d --name minion-worker --restart unless-stopped \\\n")
	d.WriteString("  -e MINION_JOIN=" + shellArg(token) + " \\\n")
	if name != "" {
		d.WriteString("  -e MINION_NAME=" + shellArg(name) + " \\\n")
	}
	d.WriteString("  -e MINION_SHARE_EXISTING=later -e MINION_VERSION=" + workerVersion() + " \\\n")
	d.WriteString("  -e MINION_STATE_DIR=/data -e MINION_CACHE_DIR=/cache \\\n")
	d.WriteString("  -v minion-worker-data:/data -v minion-worker-cache:/cache \\\n")
	d.WriteString("  --mount type=bind,source=\"$PWD/bootstrap.sh\",target=/bootstrap.sh,readonly \\\n")
	d.WriteString("  alpine:3.22 sh -c 'apk add --no-cache tmux >/dev/null && exec sh /bootstrap.sh worker up --foreground'\n")
	return d.String()
}

// joinComposeFile renders the Compose project of a Worker with MINION_JOIN. It
// holds the token: the file is a secret until the Worker has enrolled.
func joinComposeFile(token, name string) string {
	head := []string{
		"A Minion Worker in Docker, joined with a one-time join token. Before you start:",
		"1. Put bootstrap.sh next to this file. It is a file of the Minion release:",
		"   " + bootstrapURL,
		"2. Run: MINION_SHARE_EXISTING=later docker compose up -d",
		"   (the first start needs the sharing choice: all, none or later; later shares nothing",
		"   until you choose in the Hub. Leave it out on later starts: it replaces the saved choice.)",
		"3. Approve the new Worker in the Hub (Workers page): it starts pending and gets no operations until you do.",
		"MINION_JOIN holds the Hub address, the full Hub fingerprint and a one-time invitation key:",
		"the Worker connects only to the Hub with that fingerprint and enrolls with the key, which",
		"works once and expires. Treat this file as a secret until the Worker has enrolled; after that",
		"the key is used up and the Worker keeps taking the Hub address and fingerprint from MINION_JOIN.",
		"The image is built from Alpine with tmux; bootstrap.sh downloads and verifies the minion",
		"binary on the first start and keeps it in the cache volume.",
	}
	env := [][2]string{{"MINION_JOIN", yamlString(token)}}
	if name != "" {
		env = append(env, [2]string{"MINION_NAME", yamlString(strings.ReplaceAll(name, "$", "$$"))})
	}
	return composeProject(head, env, false, workerVersion())
}

// joinResult is what the join_created fragment renders: the token, once.
type joinResult struct {
	Name     string
	Expires  string
	Server   string
	HubWords string
	HubFull  string
	Token    string
	JoinCommands
}

func handleAddWorkerJoin(w http.ResponseWriter, r *http.Request, c OpCaller, _ pageUser) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name != "" && !ops.ValidWorkerName(name) {
		writeResultError(w, "The name can have at most "+strconv.Itoa(ops.MaxWorkerNameLen)+" characters, without spaces or colons.")
		return
	}
	ttl := r.PostFormValue("ttl")
	if ttl == "" {
		ttl = ops.TTL1h
	}
	server := hubURL(r)
	if server == placeholderHub || identity.ValidateJoinHubURL(server) != nil {
		writeResultError(w, "The Hub does not know the address Workers use to reach it, so no join command was created. "+
			"Set MINION_PUBLIC_URL on the Hub (for example https://hub.example.com:8443) and try again.")
		return
	}
	args := ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ttl, Name: name}
	var res ops.InvitationCreateResult
	if err := c.CallOp(r, ops.OpInvitationCreate, args, &res); err != nil {
		writeResultError(w, failureText(err))
		return
	}
	key, err := identity.ParseInviteKey(res.Key)
	if err != nil {
		writeResultError(w, "The Hub answered with an invitation key that cannot be read. Revoke it on the Invitations page and try again.")
		return
	}
	if fp := currentHubFingerprint(); fp != "" && fp != key.Fingerprint.String() {
		writeResultError(w, "The new invitation key names another Hub fingerprint than this Hub's. Revoke it on the Invitations page and check the Hub identity.")
		return
	}
	tok, err := identity.NewJoinToken(server, key)
	if err != nil {
		writeResultError(w, "No join command could be built for the address "+server+". Check MINION_PUBLIC_URL on the Hub.")
		return
	}
	out := joinResult{
		Name: res.Name, Expires: formatTime(res.ExpiresAt, "never"), Server: server,
		HubWords: key.Fingerprint.Words(), HubFull: key.Fingerprint.String(),
		Token: tok.Encode(), JoinCommands: joinCommands(tok, name),
	}
	writeFragmentTemplate(w, addWorkerFragments(), "join_created", out)
}

func init() {
	RegisterRoute(Route{Pattern: "POST " + pathAddWorkerJoin, Handler: userRoute(handleAddWorkerJoin)})
}
