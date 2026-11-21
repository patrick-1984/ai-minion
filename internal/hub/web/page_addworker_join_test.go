package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// joinFixture is a Hub with a root fingerprint and an invitation key that
// invitation_create answers with.
type joinFixture struct {
	fp  identity.Fingerprint
	key identity.InviteKey
	ops *fakeOps
}

func newJoinFixture(t *testing.T) *joinFixture {
	t.Helper()
	fp := trustFingerprint(t)
	key, err := identity.NewInviteKey(fp)
	if err != nil {
		t.Fatal(err)
	}
	onbHour := ops.Timestamp(onbNow.Add(3600e9))
	create := ops.InvitationCreateResult{ID: onbInviteID, Key: key.String(), Name: "build-1", Mode: ops.InvitationOneTime,
		Kind: ops.KindPrivate, TTL: ops.TTL1h, CreatedAt: onbTime, ExpiresAt: &onbHour}
	f := &joinFixture{fp: fp, key: key, ops: newFakeOps().reply(ops.OpInvitationCreate, create)}
	connect(t, f.ops)
	SetPublicURL(func(*http.Request) string { return "https://hub.example.com:8443" })
	t.Cleanup(func() { SetPublicURL(nil) })
	t.Cleanup(SetHubFingerprint(func() string { return fp.String() }))
	return f
}

var preRE = regexp.MustCompile(`(?s)<pre class="copy-value" id="([a-z-]+)">(.*?)</pre>`)

// pres returns the texts of the <pre> blocks of an answer by id, unescaped.
func pres(body string) map[string]string {
	out := map[string]string{}
	for _, m := range preRE.FindAllStringSubmatch(body, -1) {
		out[m[1]] = html.UnescapeString(m[2])
	}
	return out
}

func TestAddWorkerJoinCreatesAOneTimeKeyAndShowsEverythingOnce(t *testing.T) {
	old := hubVersion
	hubVersion = func() string { return "v0.18.0-rc.1" }
	t.Cleanup(func() { hubVersion = old })
	f := newJoinFixture(t)
	s := onbUser(t)
	rec := onbPost(t, s, pathAddWorkerJoin, url.Values{"ttl": {"4h"}, "name": {" build-1 "}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d\n%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	c := f.ops.called(ops.OpInvitationCreate)
	if len(c) != 1 || c[0].Args["mode"] != ops.InvitationOneTime || c[0].Args["ttl"] != "4h" || c[0].Args["name"] != "build-1" {
		t.Fatalf("create calls = %+v", c)
	}
	body := rec.Body.String()
	tok, err := identity.NewJoinToken("https://hub.example.com:8443", f.key)
	if err != nil {
		t.Fatal(err)
	}
	text := tok.Encode()
	mustHave(t, body, `>`+text+`</textarea>`, `data-copy="#join-token"`, "data-join-saved", "data-join-done", "copy-btn", "shown only once", f.fp.Words(), f.fp.String(),
		"connects only to the Hub with fingerprint", "works once and expires", "approve it on the Workers page")

	p := pres(body)
	for id, fragments := range map[string][]string{
		"join-unix":          {"MINION_JOIN_FILE=/dev/fd/3 minion worker up --name build-1", text},
		"join-system":        {"sudo env MINION_JOIN_FILE=", "minion worker up --name build-1", text},
		"join-windows":       {"minion.exe worker up --name build-1", "finally", "Remove-Item -LiteralPath $file", text},
		"join-expanded":      {"--server https://hub.example.com:8443 --hub-fingerprint " + f.fp.String(), "--invite-key /dev/fd/3", f.key.String()},
		"join-docker":        {"curl -fsSL -o bootstrap.sh.new " + bootstrapURL + " &&", "--mount type=bind,source=", "apk add --no-cache tmux", "docker run --rm -i", "--configure-only --name build-1", "--non-interactive-share all --join -", "--restart unless-stopped", "worker status --wait", "--accept-usage", text},
		"join-compose":       {`MINION_NAME: "build-1"`, `command: ["worker", "run"]`, "restart: unless-stopped"},
		"join-compose-start": {"docker compose run --build --rm -T", "--configure-only", "--join -", "docker compose up -d", "worker status --wait", "--accept-usage", text},
	} {
		if id == "join-unix" || id == "join-system" || id == "join-windows" {
			fragments = append(fragments, "--non-interactive-share all --accept-usage")
		}
		for _, fragment := range fragments {
			if !strings.Contains(p[id], fragment) {
				t.Errorf("%s lacks %s", id, fragment)
			}
		}
	}
	mustNotHave(t, p["join-compose"], text, f.key.String(), "MINION_JOIN", "--accept-usage")
	mustNotHave(t, p["join-expanded"], "--accept-usage")
	mustNotHave(t, p["join-system"], "--share-existing")
	mustNotHave(t, p["join-windows"], "--foreground")
	// The token parses back to this Hub and this key.
	got, err := identity.ParseJoinToken(text)
	if err != nil || got.HubURL != "https://hub.example.com:8443" || got.Fingerprint() != f.fp {
		t.Errorf("token parses to %v, %v", got, err)
	}
	// Never again: not on the page.
	page := onbGet(t, s, pathAddWorker).Body.String()
	mustNotHave(t, page, text, f.key.String())
}

func TestAddWorkerJoinDefaultsToOneHour(t *testing.T) {
	f := newJoinFixture(t)
	rec := onbPost(t, onbUser(t), pathAddWorkerJoin, url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "data-join-created") {
		t.Fatalf("POST = %d\n%s", rec.Code, rec.Body)
	}
	if c := f.ops.called(ops.OpInvitationCreate); len(c) != 1 || c[0].Args["ttl"] != ops.TTL1h || c[0].Args["mode"] != ops.InvitationOneTime {
		t.Fatalf("create calls = %+v", c)
	}
	p := pres(rec.Body.String())
	if strings.Contains(p["join-unix"], "--name") || strings.Contains(p["join-docker"], "MINION_NAME") {
		t.Errorf("a name without a name: %v", p)
	}
}

func TestAddWorkerJoinRefusesWithoutCreatingAKey(t *testing.T) {
	f := newJoinFixture(t)
	rec := onbPost(t, onbUser(t), pathAddWorkerJoin, url.Values{"name": {"two words"}})
	mustHave(t, rec.Body.String(), "without spaces or colons")
	// No usable public address: no key either.
	SetPublicURL(func(*http.Request) string { return "" })
	req := url.Values{}
	h := onbUser(t)
	r := httptestPostHost(t, h, pathAddWorkerJoin, req, "bad host!")
	mustHave(t, r, "MINION_PUBLIC_URL")
	if c := f.ops.called(ops.OpInvitationCreate); len(c) != 0 {
		t.Fatalf("a key was created: %+v", c)
	}
}

func TestAddWorkerJoinRefusesAKeyOfAnotherHub(t *testing.T) {
	newJoinFixture(t)
	other := trustFingerprint(t)
	t.Cleanup(SetHubFingerprint(func() string { return other.String() }))
	rec := onbPost(t, onbUser(t), pathAddWorkerJoin, url.Values{})
	mustHave(t, rec.Body.String(), "another Hub fingerprint")
	mustNotHave(t, rec.Body.String(), "mnj1-")
}

func TestAddWorkerJoinShowsTheErrorOfTheHub(t *testing.T) {
	f := newFakeOps().on(ops.OpInvitationCreate, func(map[string]any) (any, error) {
		return nil, &OpError{Code: "invalid_argument", Message: "ttl: not allowed"}
	})
	connect(t, f)
	SetPublicURL(func(*http.Request) string { return "https://hub.example.com" })
	t.Cleanup(func() { SetPublicURL(nil) })
	rec := onbPost(t, onbUser(t), pathAddWorkerJoin, url.Values{"ttl": {"1m"}})
	mustNotHave(t, rec.Body.String(), "data-join-created", "mnj1-")
}

func TestAddWorkerPageOffersTheOneStepForm(t *testing.T) {
	newJoinFixture(t)
	body := onbGet(t, onbUser(t), pathAddWorker).Body.String()
	mustHave(t, body, "Add a Worker in one step", `hx-post="/workers/add/join"`, `<option value="1h" selected>`, "Pairing with a code instead?")
	mustNotHave(t, body, `value="1m"`, `value="unlimited"`)
	admin := onbGet(t, onbAdmin(t), pathAddWorker).Body.String()
	mustHave(t, admin, `value="1m"`, `<option value="1h" selected>`)
}

// Run the commands as shell input, like a paste at the prompt. The fake
// clients capture arguments separately from credentials carried by fd/file/stdin.
func TestJoinShellCommandsPassTheTokenIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	key, err := identity.NewInviteKey(trustFingerprint(t))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := identity.NewJoinToken("https://hub.example.com", key)
	if err != nil {
		t.Fatal(err)
	}
	jc := joinCommands(tok, "it's-a-name")
	bin, out := t.TempDir(), t.TempDir()
	fakes := map[string]string{
		"minion": `printf '%s\n' "$@" >> "$OUT/argv"
if [ -n "$MINION_JOIN_FILE" ]; then
  cat "$MINION_JOIN_FILE" >> "$OUT/join"
  if [ "$MINION_JOIN_FILE" != /dev/fd/3 ]; then
    ls -ld "$MINION_JOIN_FILE" | cut -c1-10 > "$OUT/joinmode"
    echo "$MINION_JOIN_FILE" > "$OUT/temppath"
  fi
else
  cat /dev/fd/3 > "$OUT/invite"
fi
`,
		"curl": `[ -z "$CURL_FAIL" ] || exit 22
while [ $# -gt 0 ]; do if [ "$1" = -o ]; then echo '#!/bin/sh' > "$2"; fi; shift; done
`,
		"docker": `printf '%s\n' "$@" >> "$OUT/argv"
case " $* " in
*" container inspect "*) [ -n "$CONTAINER_EXISTS" ] ;;
*" build -q "*) cat >/dev/null; echo alpine-image ;;
*" --configure-only "*) cat >> "$OUT/join"; [ -z "$ENROLL_FAIL" ] ;;
*" status --wait "*) echo ready >> "$OUT/status" ;;
*) exit 0 ;;
esac
`,
		"sudo": `printf '%s\n' "$@" >> "$OUT/argv"
exec "$@"
`,
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(cmdText string, env ...string) error {
		cmd := exec.Command("/bin/sh")
		cmd.Stdin = strings.NewReader(cmdText)
		cmd.Dir = out
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "OUT=" + out, "TMPDIR=" + t.TempDir()}, env...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, b)
		}
		return nil
	}
	if err := run(jc.Docker, "CURL_FAIL=1"); err == nil {
		t.Error("the docker block succeeded although curl failed")
	}
	if _, err := os.Stat(filepath.Join(out, "argv")); !os.IsNotExist(err) {
		t.Fatal("docker ran after a failed download")
	}
	if err := run(jc.Docker, "CONTAINER_EXISTS=1"); err == nil || !strings.Contains(err.Error(), "A container named minion-worker exists") {
		t.Errorf("the docker block did not stop before enrolling: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "argv")); strings.Contains(string(b), "--configure-only") {
		t.Fatal("docker enrolled although the container name was taken")
	}
	if err := os.Remove(filepath.Join(out, "argv")); err != nil {
		t.Fatal(err)
	}
	for _, cmdText := range []string{jc.Unix, jc.Expanded, jc.Docker, jc.System, jc.ComposeStart} {
		if err := run(cmdText); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("join"); got != strings.Repeat(tok.Encode()+"\n", 4) || strings.TrimSpace(read("invite")) != key.String() {
		t.Error("a token or expanded invitation did not arrive intact")
	}
	args := read("argv")
	mustNotHave(t, args, tok.Encode(), key.String())
	mustHave(t, args, "worker\nup\n--name\nit's-a-name", "worker\nrun", "--restart\nunless-stopped", "type=bind,source="+out+"/bootstrap.sh", "--invite-key\n/dev/fd/3", "env\nMINION_JOIN_FILE=")
	if read("joinmode") != "-rw-------\n" {
		t.Error("insecure temporary credential permissions")
	}
	if _, err := os.Stat(strings.TrimSpace(read("temppath"))); !os.IsNotExist(err) {
		t.Error("credential file survived the command")
	}
	if err := run(jc.Docker, "ENROLL_FAIL=1"); err == nil {
		t.Fatal("docker enrollment failure lost")
	}
	if got := read("status"); got != "ready\nready\n" {
		t.Errorf("unexpected status checks: %q", got)
	}
	mustHave(t, jc.Windows, "minion.exe worker up --name 'it''s-a-name'")
}

func TestPSArgDoublesEverySingleQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":           "plain",
		"it's":            "'it''s'",
		"a\u2018b\u2019c": "'a\u2018\u2018b\u2019\u2019c'",
		"x\u201ay\u201bz": "'x\u201a\u201ay\u201b\u201bz'",
		"a b":             "'a b'",
		"@x":              "'@x'",
	} {
		if got := psArg(in); got != want {
			t.Errorf("psArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContainerWorkerRunsTheHubsRelease(t *testing.T) {
	old := hubVersion
	t.Cleanup(func() { hubVersion = old })
	hubVersion = func() string { return "v0.12.0-rc.1" }
	if d := dockerRunJoin("mnj1-x", ""); !strings.Contains(d, "-e MINION_VERSION=v0.12.0-rc.1 ") {
		t.Errorf("docker run:\n%s", d)
	}
	if c := joinComposeFile(""); !strings.Contains(c, "MINION_VERSION: ${MINION_VERSION:-v0.12.0-rc.1}") {
		t.Errorf("compose:\n%s", c)
	}
	hubVersion = func() string { return "dev" }
	if d := dockerRunJoin("mnj1-x", ""); !strings.Contains(d, "MINION_VERSION=latest") {
		t.Errorf("a development Hub:\n%s", d)
	}
}

// With a Docker CLI at hand, `docker compose config` accepts the join project.
func TestJoinComposeProjectIsValidCompose(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("no docker CLI")
	}
	if err := exec.Command(docker, "compose", "version").Run(); err != nil {
		t.Skip("no docker compose plugin")
	}
	const token = "mnj1-eyJodWIiOiJodHRwczovL2h1Yi5leGFtcGxlIn0"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(joinComposeFile("build-1")), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(docker, "compose", "-f", filepath.Join(dir, "compose.yaml"), "config", "--format", "json")
	cmd.Env = append(os.Environ(), "MINION_VERSION=", "MINION_SHARE_EXISTING=later")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, b)
	}
	var cfg struct {
		Services map[string]struct {
			Environment map[string]string `json:"environment"`
			Secrets     []any             `json:"secrets"`
		} `json:"services"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	env := cfg.Services["minion-worker"].Environment
	if bytes.Contains(b, []byte(token)) || env["MINION_JOIN"] != "" || env["MINION_NAME"] != "build-1" || env["MINION_SHARE_EXISTING"] != "later" {
		t.Errorf("environment = %v", env)
	}
	if len(cfg.Services["minion-worker"].Secrets) != 0 || bytes.Contains(b, []byte("invite_key")) {
		t.Errorf("the join project uses the invitation file:\n%s", b)
	}
}

func TestInviteCommandsPinTheHub(t *testing.T) {
	key, err := identity.NewInviteKey(trustFingerprint(t))
	if err != nil {
		t.Fatal(err)
	}
	got := inviteCommands("https://hub.example.com", key.String())
	fp := key.Fingerprint.String()
	if want := "minion worker up --server https://hub.example.com --hub-fingerprint " + fp + " --invite-key " + key.String(); got.OneLiner != want {
		t.Errorf("one-liner = %q", got.OneLiner)
	}
	if !strings.Contains(got.FileVariant, "--hub-fingerprint "+fp+` --invite-key "$dir/invite.key"`) {
		t.Errorf("file variant:\n%s", got.FileVariant)
	}
	if !strings.Contains(got.Compose, "MINION_HUB_FINGERPRINT: "+fp) || strings.Contains(got.Compose, key.String()) {
		t.Errorf("compose:\n%s", got.Compose)
	}
}

// httptestPostHost is onbPost with the Host header host; it returns the body.
func httptestPostHost(t *testing.T, h http.Handler, path string, form url.Values, host string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}
