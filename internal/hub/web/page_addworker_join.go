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
	Unix, System, Windows string
	// Expanded also works on older Workers with invitation keys.
	Expanded string
	// Compose holds no credentials. ComposeStart enrolls with the one-time token.
	Docker, Compose, ComposeStart string
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
	const initialShare = " --non-interactive-share all --accept-usage"
	text := tok.Encode()
	var unixName, psName string
	if name != "" {
		unixName = " --name " + shellArg(name)
		psName = " --name " + psArg(name)
	}
	// Keep stdin on the terminal for the sharing question. The shell gives
	// the Worker a separate read-only descriptor for the one-use credential.
	return JoinCommands{
		Unix:    "MINION_JOIN_FILE=/dev/fd/3 minion worker up" + unixName + initialShare + credentialFD(text),
		System:  systemJoin(text, unixName+initialShare),
		Windows: windowsJoin(text, psName+initialShare),
		Expanded: "minion worker up --server " + shellArg(tok.HubURL) + " --hub-fingerprint " + tok.Fingerprint().String() +
			unixName + " --invite-key /dev/fd/3" + credentialFD(tok.Invite.String()),
		Docker:       dockerRunJoin(text, name),
		Compose:      joinComposeFile(name),
		ComposeStart: composeStart(text),
	}
}

// credentialFD sends the credential on descriptor 3, leaving stdin available.
func credentialFD(secret string) string {
	return " 3<<'MINION_CREDENTIAL'\n" + secret + "\nMINION_CREDENTIAL\n"
}

// sudo closes extra descriptors. Give it just a temporary file's name and
// leave the terminal free for choosing a target account and what to share.
func systemJoin(token, nameArg string) string {
	return `(
  umask 077 &&
  dir=$(mktemp -d "${TMPDIR:-/tmp}/minion-join.XXXXXX") &&
  trap 'rm -rf "$dir"' EXIT &&
  cat > "$dir/join" <<'MINION_CREDENTIAL' &&
` + token + `
MINION_CREDENTIAL
  sudo env MINION_JOIN_FILE="$dir/join" minion worker up` + nameArg + "\n)\n"
}

// Preserve console input for the Windows wizard and restore the calling
// shell's environment even if PowerShell or the Worker reports a failure.
func windowsJoin(token, nameArg string) string {
	return `& {
  $previous = $env:MINION_JOIN_FILE
  $file = [IO.Path]::GetTempFileName()
  $rc = 0
  try {
    [IO.File]::WriteAllText($file, '` + token + `')
    $env:MINION_JOIN_FILE = $file
    minion.exe worker up` + nameArg + `
    $rc = $LASTEXITCODE
  } finally {
    $env:MINION_JOIN_FILE = $previous
    Remove-Item -LiteralPath $file
  }
  if ($rc -ne 0) { Write-Error "minion worker up failed (exit code $rc)" }
}`
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

// Docker supervises the foreground process. Only the disposable configuration
// container gets the one-use token, on stdin. Both containers see the same
// persistent state, which holds the public Hub settings and enrolled identity.
const downloadBootstrap = "curl -fsSL -o bootstrap.sh.new " + bootstrapURL + " &&\nmv -f bootstrap.sh.new bootstrap.sh &&\nchmod 0644 bootstrap.sh &&\n"

func dockerRunJoin(token, name string) string {
	var nameArg string
	if name != "" {
		nameArg = " --name " + shellArg(name)
	}
	tag := workerVersion()
	accept := usageInstallerArgs(tag, "--accept-usage")
	// Set positional parameters inside a subshell so both runs get the same
	// state, cache, release and image without changing the caller's shell.
	return downloadBootstrap + `(
  if docker container inspect minion-worker >/dev/null 2>&1; then
    echo "A container named minion-worker exists: remove it or rename the new one" >&2
    exit 1
  fi &&
  image=$(docker build -q - <<'DOCKERFILE'
FROM alpine:3.22
RUN apk add --no-cache tmux
DOCKERFILE
  ) &&
  set -- -e MINION_STATE_DIR=/data -e MINION_CACHE_DIR=/cache -e MINION_VERSION=` + tag + ` \
    -v minion-worker-data:/data -v minion-worker-cache:/cache \
    --mount type=bind,source="$PWD/bootstrap.sh",target=/bootstrap.sh,readonly "$image" sh /bootstrap.sh &&
  docker run --rm -i "$@" worker up --configure-only` + nameArg + ` --non-interactive-share all --join -` + accept + ` <<'MINION_CREDENTIAL' &&
` + token + `
MINION_CREDENTIAL
  docker run -d --name minion-worker --restart unless-stopped "$@" worker run &&
  docker exec minion-worker sh /bootstrap.sh worker status --wait &&
  printf '%s\n' 'docker logs minion-worker' 'docker stop minion-worker' 'docker start minion-worker'
)
`
}

func joinComposeFile(name string) string {
	head := []string{
		"Save as compose.yaml and run the enrollment command from the Add worker page once.",
		"The enrolled identity and settings persist in worker-data.",
	}
	var env [][2]string
	if name != "" {
		env = append(env, [2]string{"MINION_NAME", yamlString(strings.ReplaceAll(name, "$", "$$"))})
	}
	return composeProject(head, env, false, workerVersion())
}

func composeStart(token string) string {
	accept := usageInstallerArgs(workerVersion(), "--accept-usage")
	return downloadBootstrap + `docker compose run --build --rm -T minion-worker worker up --configure-only --non-interactive-share all --join -` + accept + ` <<'MINION_CREDENTIAL' &&
` + token + `
MINION_CREDENTIAL
docker compose up -d &&
docker compose exec -T minion-worker sh /bootstrap.sh worker status --wait &&
printf '%s\n' 'docker compose logs minion-worker' 'docker compose stop minion-worker' 'docker compose start minion-worker'
`
}

// joinResult is what the join_created fragment renders: the token, once.
type joinResult struct {
	Name            string
	Expires         string
	Server          string
	HubWords        string
	HubFull         string
	Token           string
	UsageDisclosure string
	JoinCommands
	hostedJoin
	Fold joinFolds
}

// hostedJoin contains the release installer variants for a newly issued token.
type hostedJoin struct {
	OneLiner, Masked, Service, Root, ComposeInstall, WindowsInstall, WindowsMasked FoldBlock
	Verified, VerifiedMasked                                                       FoldBlock
	HasInstaller, HasVerified, KeyedOK                                             bool
}

func hostedJoinCommands(token, name string, keyed bool) hostedJoin {
	out := hostedJoin{KeyedOK: keyed}
	tag := installerTag()
	if tag == "" {
		return out
	}
	out.HasInstaller = true
	out.Masked = foldBlock("join-masked", "Copy the command", pipeLine(tag, "", name))
	if !keyed {
		token = ""
	} else {
		out.OneLiner = foldBlock("join-oneliner", "Copy the command", pipeLine(tag, token, name))
	}
	out.Service = foldBlock("join-service-install", "Copy the command", installerModeLine(tag, token, name, "--service"))
	out.Root = foldBlock("join-root-install", "Copy the command", installerModeLine(tag, token, name, "--root"))
	out.ComposeInstall = foldBlock("join-compose-install", "Copy the command", installerModeLine(tag, token, name, "--compose"))
	out.WindowsMasked = foldBlock("join-windows-masked", "Copy the command", windowsInstallerLine(tag, "", name))
	out.WindowsInstall = foldBlock("join-windows-install", "Copy the command", windowsInstallerLine(tag, token, name))
	warmInstaller()
	if offer, ok := cachedInstaller(); ok {
		out.HasVerified = true
		out.VerifiedMasked = foldBlock("join-verified-masked", "Copy the command", installLine(offer, "", name))
		if keyed {
			out.Verified = foldBlock("join-verified", "Copy the command", installLine(offer, token, name))
		}
	}
	return out
}

// joinFolds are the long commands as foldable blocks.
type joinFolds struct {
	Unix, System, Windows, Docker, Compose, ComposeStart, Expanded FoldBlock
}

func foldJoin(c JoinCommands) joinFolds {
	return joinFolds{
		Unix:         foldBlock("join-unix", "Copy the command", c.Unix),
		System:       foldBlock("join-system", "Copy the commands", c.System),
		Windows:      foldBlock("join-windows", "Copy the command", c.Windows),
		Docker:       foldBlock("join-docker", "Copy the commands", c.Docker),
		Compose:      foldBlock("join-compose", "Copy the Compose file", c.Compose),
		ComposeStart: foldBlock("join-compose-start", "Copy the enrollment command", c.ComposeStart),
		Expanded:     foldBlock("join-expanded", "Copy the command", c.Expanded),
	}
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
	// The one name is the key's label and the Worker's name: the Hub stores
	// it for the Worker whatever name the Worker announces (a Worker given
	// only the token, like the Android app, announces its own).
	args := ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ttl, Name: name, WorkerName: name}
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
		Token: tok.Encode(), UsageDisclosure: ops.UsageDisclosure,
		// One use is fixed above; a lifetime must be finite for a command
		// that holds the token (also for an administrator).
		hostedJoin: hostedJoinCommands(tok.Encode(), name, res.ExpiresAt != nil && ttl != ops.TTLUnlimited),
	}
	if out.KeyedOK {
		out.JoinCommands = joinCommands(tok, name)
		out.Fold = foldJoin(out.JoinCommands)
	}
	writeFragmentTemplate(w, addWorkerFragments(), "join_created", out)
}

func init() {
	RegisterRoute(Route{Pattern: "POST " + pathAddWorkerJoin, Handler: userRoute(handleAddWorkerJoin)})
}
