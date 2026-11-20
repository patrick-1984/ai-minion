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
	want := map[string]string{
		"join-unix":    "minion worker up --name build-1 --join " + text,
		"join-windows": "minion.exe worker up --foreground --name build-1 --join " + text,
		"join-expanded": "minion worker up --server https://hub.example.com:8443 --hub-fingerprint " + f.fp.String() +
			" --name build-1 --invite-key " + f.key.String(),
	}
	for id, w := range want {
		if p[id] != w {
			t.Errorf("%s = %q, want %q", id, p[id], w)
		}
	}
	for _, w := range []string{"-e MINION_JOIN=" + text, "-e MINION_NAME=build-1", "-e MINION_SHARE_EXISTING=later",
		"curl -fsSL -o bootstrap.sh.new " + bootstrapURL + " &&", "--mount type=bind,source=", "exec sh /bootstrap.sh worker up --foreground", "apk add --no-cache tmux"} {
		if !strings.Contains(p["join-docker"], w) {
			t.Errorf("docker lacks %q:\n%s", w, p["join-docker"])
		}
	}
	for _, w := range []string{`MINION_JOIN: "` + text + `"`, `MINION_NAME: "build-1"`, "MINION_SHARE_EXISTING=later docker compose up -d"} {
		if !strings.Contains(p["join-compose"], w) {
			t.Errorf("compose lacks %q:\n%s", w, p["join-compose"])
		}
	}
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

// The shell commands, run by a real POSIX shell with fake minion, curl and
// docker on the PATH: the token arrives as one argument, unchanged.
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
		"minion": "printf '%s\\n' \"$@\" >> \"$OUT/minion\"\n" +
			"[ -n \"$MINION_JOIN_FILE\" ] && cat \"$MINION_JOIN_FILE\" > \"$OUT/joinfile\" && ls -ld \"$MINION_JOIN_FILE\" | cut -c1-10 > \"$OUT/joinmode\"\nexit 0\n",
		// curl writes the file named by -o, or fails like a failed download.
		"curl": "printf '%s\\n' \"$@\" >> \"$OUT/curl\"\n[ -n \"$CURL_FAIL\" ] && exit 22\n" +
			"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then echo '#!/bin/sh' > \"$2\"; fi; shift; done\n",
		"docker": "printf '%s\\n' \"$@\" >> \"$OUT/docker\"\n",
		// sudo records its argv and runs the command.
		"sudo": "printf '%s\\n' \"$@\" >> \"$OUT/sudo\"\nexec \"$@\"\n",
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(cmdText string, env ...string) error {
		cmd := exec.Command("/bin/sh", "-c", cmdText)
		cmd.Dir = out
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "OUT=" + out, "TMPDIR=" + t.TempDir()}, env...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, b)
		}
		return nil
	}
	// A failed download starts no container.
	if err := run(jc.Docker, "CURL_FAIL=1"); err == nil {
		t.Error("the docker block succeeded although curl failed")
	}
	if _, err := os.Stat(filepath.Join(out, "docker")); err == nil {
		t.Error("docker ran after a failed download")
	}
	for _, cmdText := range []string{jc.Unix, jc.Expanded, jc.Docker, jc.System} {
		if err := run(cmdText); err != nil {
			t.Fatalf("%s: %v", cmdText, err)
		}
	}
	read := func(name string) []string {
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	minion := read("minion")
	wantUnix := []string{"worker", "up", "--name", "it's-a-name", "--join", tok.Encode()}
	wantExp := []string{"worker", "up", "--server", "https://hub.example.com", "--hub-fingerprint", key.Fingerprint.String(),
		"--name", "it's-a-name", "--invite-key", key.String()}
	wantSys := []string{"worker", "up", "--share-existing", "later", "--name", "it's-a-name"}
	if strings.Join(minion, "|") != strings.Join(append(append(wantUnix, wantExp...), wantSys...), "|") {
		t.Errorf("minion got %q", minion)
	}
	docker := strings.Join(read("docker"), "|")
	for _, w := range []string{"MINION_JOIN=" + tok.Encode(), "MINION_NAME=it's-a-name", "type=bind,source=" + out + "/bootstrap.sh,target=/bootstrap.sh,readonly", "alpine:3.22"} {
		if !strings.Contains(docker, w) {
			t.Errorf("docker lacks %q: %s", w, docker)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "bootstrap.sh")); err != nil {
		t.Errorf("bootstrap.sh was not put in place: %v", err)
	}
	// The sudo variant: the token reaches the Worker through a private file,
	// never through sudo's argv.
	if got := strings.TrimSpace(strings.Join(read("joinfile"), "\n")); got != tok.Encode() {
		t.Errorf("the join file holds %q", got)
	}
	if mode := strings.Join(read("joinmode"), ""); mode != "-rw-------" {
		t.Errorf("the join file mode is %s", mode)
	}
	if sudo := strings.Join(read("sudo"), "|"); strings.Contains(sudo, tok.Encode()) || !strings.HasPrefix(sudo, "env|MINION_JOIN_FILE=") {
		t.Errorf("sudo argv = %s", sudo)
	}
	if jc.Windows != "minion.exe worker up --foreground --name 'it''s-a-name' --join "+tok.Encode() {
		t.Errorf("windows = %q", jc.Windows)
	}
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
	if c := joinComposeFile("mnj1-x", ""); !strings.Contains(c, "MINION_VERSION: ${MINION_VERSION:-v0.12.0-rc.1}") {
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
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(joinComposeFile(token, "build-1")), 0o644); err != nil {
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
	if env["MINION_JOIN"] != token || env["MINION_NAME"] != "build-1" || env["MINION_SHARE_EXISTING"] != "later" {
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
