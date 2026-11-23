package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
)

const (
	testInstallerDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testChecksumsDigest = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// withInstaller makes this Hub a release v1.2.3 whose signed manifest lists the
// installer digest sha.
func withInstaller(t *testing.T, sha string) {
	t.Helper()
	old := hubVersion
	hubVersion = func() string { return "v1.2.3" }
	t.Cleanup(func() { hubVersion = old })
	t.Cleanup(SetInstallerSource(func(_ context.Context, tag string) (version.InstallerInfo, error) {
		if tag != "v1.2.3" {
			return version.InstallerInfo{}, errors.New("unexpected tag " + tag)
		}
		return version.InstallerInfo{InstallerSHA256: sha, ChecksumsSHA256: testChecksumsDigest}, nil
	}))
	warmInstaller()
	for i := 0; i < 200; i++ {
		installerMu.Lock()
		done := installerState.ok || !installerState.failedAt.IsZero()
		installerMu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the installer lookup did not finish")
}

func joinPost(t *testing.T, s *Server, ttl string) string {
	t.Helper()
	rec := onbPost(t, s, pathAddWorkerJoin, url.Values{"ttl": {ttl}})
	if rec.Code != 200 {
		t.Fatalf("POST = %d\n%s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestAddWorkerShowsTheVerifiedOneLinerAtTheTop(t *testing.T) {
	f := newJoinFixture(t)
	withInstaller(t, testInstallerDigest)
	rec := onbPost(t, onbUser(t), pathAddWorkerJoin, url.Values{"ttl": {"1h"}, "name": {"build-1"}})
	body := rec.Body.String()
	tok, err := identity.NewJoinToken("https://hub.example.com:8443", f.key)
	if err != nil {
		t.Fatal(err)
	}
	p := pres(body)
	for id, parts := range map[string][]string{
		"join-oneliner":        {"install.sh) && case", "MINION_JOIN=" + tok.Encode(), "sh -s -- --join --name build-1 --accept-usage"},
		"join-masked":          {"MINION_JOIN= sh -s -- --join --name build-1 --accept-usage"},
		"join-service-install": {"--service --name build-1 --accept-usage"},
		"join-root-install":    {"--root --name build-1 --accept-usage"},
		"join-compose-install": {"--compose --name build-1 --accept-usage"},
		"join-windows-install": {"try { $env:MINION_JOIN='" + tok.Encode() + "'; [Net.ServicePointManager]", "install.ps1", "-Join -Version v1.2.3 -Name build-1 -AcceptUsage } finally { Remove-Item Env:MINION_JOIN"},
		"join-windows-masked":  {"$j = $env:MINION_JOIN; try { $env:MINION_JOIN=''; [Net.ServicePointManager]", "install.ps1", "-Join -Version v1.2.3 -Name build-1 -AcceptUsage } finally { $env:MINION_JOIN = $j }"},
	} {
		for _, part := range parts {
			if !strings.Contains(p[id], part) || strings.Contains(p[id], "\n") {
				t.Errorf("%s lacks %q or is not one line", id, part)
			}
		}
	}
	mustHave(t, body, "<details data-join-verified><summary>Verified variant (checks the installer's SHA-256 from the signed release first)")
	line := p["join-verified"]
	for _, want := range []string{
		"curl -fsSL https://github.com/patrick-1984/ai-minion/releases/download/v1.2.3/install.sh -o \"$f\"",
		`[ -s "$f" ]`, "grep -q '^" + testInstallerDigest + " '",
		"MINION_VERSION=v1.2.3 MINION_CHECKSUMS_SHA256=" + testChecksumsDigest, "MINION_JOIN=" + tok.Encode() + " sh \"$f\" --join --name build-1 --accept-usage; s=$?; rm -f \"$f\"; exit $s)",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("one-liner lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("the one-liner is not one line:\n%s", line)
	}
	masked := p["join-verified-masked"]
	if !strings.Contains(masked, testInstallerDigest) || !strings.Contains(masked, `MINION_JOIN= sh "$f" --join --name build-1 --accept-usage;`) ||
		strings.Contains(masked, "mnj1") || strings.Contains(masked, tok.Encode()) || strings.Contains(masked, "\n") {
		t.Errorf("masked variant:\n%s", masked)
	}
	if strings.Contains(p["join-windows-masked"], "mnj1") || strings.Contains(p["join-windows-masked"], tok.Encode()) || !strings.Contains(p["join-windows-masked"], "$env:MINION_JOIN=''") {
		t.Errorf("the token-free Windows command carries a token or does not clear an inherited one: %s", p["join-windows-masked"])
	}
	// Windows is a visible option after Docker Compose; only the token-free variant is folded.
	mustHave(t, body, "<h3>Windows PowerShell</h3>", "<summary>Windows PowerShell without the token in history</summary>")
	mustNotHave(t, body, "<summary>Windows PowerShell</summary>")
	if strings.Index(body, `id="join-compose-install"`) >= strings.Index(body, "<h3>Windows PowerShell</h3>") ||
		strings.Index(body, "<h3>Windows PowerShell</h3>") >= strings.Index(body, `id="join-windows-install"`) ||
		strings.Index(body, `id="join-windows-install"`) >= strings.Index(body, "without the token in history</summary>") {
		t.Error("Windows PowerShell must follow Docker Compose, with the token-free variant folded after it")
	}
	// The key comes first, then the one-liner, then the long variants.
	for _, later := range []string{`id="join-verified"`, `id="join-unix"`, `id="join-docker"`} {
		if strings.Index(body, `id="join-oneliner"`) > strings.Index(body, later) {
			t.Errorf("the one-liner comes after %s", later)
		}
	}
	if strings.Index(body, `id="join-token"`) > strings.Index(body, `id="join-oneliner"`) {
		t.Error("the join token must come above the options")
	}
	// No live region around the token.
	mustNotHave(t, body, `role="status" data-join-created`, `aria-live`)
	mustHave(t, body, "It needs systemd; in a container (a Coder workspace, Docker) there is none, so choose Run as me.",
		"stays in your PowerShell history", "shares all tmux sessions", "--share none", "data-join-usage", "including unshared sessions", "session names, folders", "no permission to read or control terminals")
	if strings.Index(body, "data-join-usage") > strings.Index(body, `id="join-oneliner"`) {
		t.Fatal("the usage disclosure must appear before commands that accept it")
	}
}

func TestAddWorkerWithoutAKnownInstallerShowsTheManualCommands(t *testing.T) {
	newJoinFixture(t)
	old := hubVersion
	hubVersion = func() string { return "dev" }
	t.Cleanup(func() { hubVersion = old })
	t.Cleanup(SetInstallerSource(func(context.Context, string) (version.InstallerInfo, error) {
		return version.InstallerInfo{InstallerSHA256: testInstallerDigest, ChecksumsSHA256: testChecksumsDigest}, nil
	}))
	body := joinPost(t, onbUser(t), "1h")
	mustHave(t, body, "data-join-manual", `id="join-unix"`, `id="join-docker"`)
	mustNotHave(t, body, `id="join-oneliner"`, `id="join-masked"`, testInstallerDigest)

	// A release whose installer digest cannot be verified still gets the
	// simple commands, without the verified variant.
	withInstaller(t, "not-a-digest")
	b := joinPost(t, onbUser(t), "1h")
	mustHave(t, b, `id="join-oneliner"`)
	mustNotHave(t, b, "data-join-verified", `id="join-verified"`)
}

func TestInvitationShowsUsageDisclosureBeforeInstallerCommands(t *testing.T) {
	newJoinFixture(t)
	withInstaller(t, testInstallerDigest)
	rec := onbPost(t, onbUser(t), pathInvitations, url.Values{"ttl": {"1h"}, "mode": {ops.InvitationOneTime}})
	if rec.Code != 200 {
		t.Fatalf("POST invitation: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	disclosure := strings.Index(body, `<div class="notice notice-info" data-join-usage>`)
	hosted := strings.Index(body, `<div data-join-oneline>`)
	if disclosure < 0 || hosted <= disclosure {
		t.Fatal("the invitation must show the usage disclosure before its installer commands")
	}
	mustHave(t, body[disclosure:hosted], "<code>--accept-usage</code>", "<code>-AcceptUsage</code>",
		ops.UsageDisclosure, "no permission to read or control terminals")

	commands := pres(body)
	for _, id := range []string{
		"join-masked", "join-service-install", "join-root-install", "join-compose-install", "join-verified-masked",
	} {
		mustHave(t, commands[id], "--accept-usage")
	}
	mustHave(t, commands["join-windows-masked"], "-AcceptUsage")
}

func TestAddWorkerUnlimitedKeyOnlyGetsTheMaskedCommand(t *testing.T) {
	f := newJoinFixture(t)
	f.ops.reply(ops.OpInvitationCreate, ops.InvitationCreateResult{ID: onbInviteID, Key: f.key.String(), Name: "n",
		Mode: ops.InvitationOneTime, Kind: ops.KindPrivate, TTL: ops.TTLUnlimited, CreatedAt: onbTime})
	withInstaller(t, testInstallerDigest)
	body := joinPost(t, onbAdmin(t), ops.TTLUnlimited)
	tok, _ := identity.NewJoinToken("https://hub.example.com:8443", f.key)
	mustHave(t, body, "data-join-unlimited", `id="join-masked"`, `id="join-verified-masked"`, `id="join-token"`)
	mustNotHave(t, body, `id="join-oneliner"`, `id="join-verified"`, `id="join-unix"`, `id="join-system"`, `id="join-docker"`, `id="join-expanded"`, "MINION_JOIN='", "MINION_JOIN=mnj1")
	// The token appears once: in the field to paste at the prompt.
	if n := strings.Count(body, tok.Encode()); n != 1 {
		t.Errorf("the token appears %d times", n)
	}
	// One use and a lifetime are the Hub's; the page asks for exactly that.
	c := f.ops.called(ops.OpInvitationCreate)
	if len(c) != 1 || c[0].Args["mode"] != ops.InvitationOneTime {
		t.Errorf("create calls: %+v", c)
	}
}

func TestAddWorkerFoldsLongCommandsAndKeepsCopyVisible(t *testing.T) {
	f := newJoinFixture(t)
	body := joinPost(t, onbUser(t), "1h")
	tok, _ := identity.NewJoinToken("https://hub.example.com:8443", f.key)
	mustHave(t, body, `<details class="fold" data-fold><summary>`, `<code class="fold-preview" aria-hidden="true">`, "Show all ")
	// The copy button is beside the fold, not inside it, and points at the full text.
	re := regexp.MustCompile(`(?s)<details class="fold" data-fold>.*?</details><button type="button" class="copy-btn fold-copy" data-copy="#(join-[a-z-]+)" data-copy-target="#(join-[a-z-]+)"`)
	m := re.FindAllStringSubmatch(body, -1)
	if len(m) < 3 {
		t.Fatalf("folded blocks with a copy button: %d", len(m))
	}
	for _, x := range m {
		if x[1] != x[2] || !strings.Contains(body, `<pre class="copy-value" id="`+x[1]+`">`) {
			t.Errorf("copy target %q/%q has no full text", x[1], x[2])
		}
	}
	// The compose file is long: the preview holds its first 3 lines only; the
	// full text is in the pre. Tokens stay fully visible in their own field.
	comp := foldBlock("x", "y", "a\nb\nc\nd\n")
	if !comp.Folded || comp.Preview != "a\nb\nc" || comp.Lines != 4 {
		t.Errorf("fold = %+v", comp)
	}
	if short := foldBlock("x", "y", "a\nb\nc\n"); short.Folded {
		t.Error("a three-line block is folded")
	}
	mustHave(t, body, `>`+tok.Encode()+`</textarea>`)
}

// The one-liner really runs: it verifies the download, passes the token only in
// the environment, and runs nothing when the download is wrong.
func TestOneLinerVerifiesBeforeRunningAndKeepsTheTokenOutOfArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	key, err := identity.NewInviteKey(trustFingerprint(t))
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := identity.NewJoinToken("https://hub.example.com", key)
	installer := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OUT/argv\"\nprintf '%s\\n' \"$MINION_JOIN\" > \"$OUT/join\"\nprintf '%s\\n' \"$MINION_VERSION\" > \"$OUT/ver\"\n"
	sum := sha256.Sum256([]byte(installer))
	offer := InstallerOffer{Tag: "v1.2.3", URL: releaseDownloadBase + "v1.2.3/install.sh", SHA256: hex.EncodeToString(sum[:]), ChecksumsSHA256: testChecksumsDigest}

	run := func(served string, token string) (out string, ran bool) {
		bin, dir := t.TempDir(), t.TempDir()
		src := filepath.Join(dir, "served")
		if err := os.WriteFile(src, []byte(served), 0o644); err != nil {
			t.Fatal(err)
		}
		curl := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && dest=$2; shift; done\ncp \"$SERVED\" \"$dest\"\n"
		if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", installLine(offer, token, ""))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "OUT="+dir, "SERVED="+src, "TMPDIR="+dir)
		_ = cmd.Run()
		_, err := os.Stat(filepath.Join(dir, "argv"))
		leftovers, _ := filepath.Glob(filepath.Join(dir, "minion-install.*"))
		if len(leftovers) != 0 {
			t.Errorf("the staging file was not removed: %v", leftovers)
		}
		return dir, err == nil
	}
	dir, ran := run(installer, tok.Encode())
	if !ran {
		t.Fatal("the verified installer did not run")
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	join, _ := os.ReadFile(filepath.Join(dir, "join"))
	ver, _ := os.ReadFile(filepath.Join(dir, "ver"))
	if strings.TrimSpace(string(argv)) != "--join\n--accept-usage" || strings.Contains(string(argv), "mnj1") ||
		strings.TrimSpace(string(join)) != tok.Encode() || strings.TrimSpace(string(ver)) != "v1.2.3" {
		t.Errorf("argv %q join %q ver %q", argv, join, ver)
	}
	for name, served := range map[string]string{"tampered": installer + "# evil\n", "truncated": installer[:20], "empty": ""} {
		if _, ran := run(served, tok.Encode()); ran {
			t.Errorf("%s download was run", name)
		}
	}
}

// A release older than the first one with --join, and a development build,
// get no one-line command.
func TestAddWorkerOneLinersNeedAnInstallerThatCanJoin(t *testing.T) {
	newJoinFixture(t)
	for _, v := range []string{"v0.16.0", "v0.16.9", "dev", "v0.0.0-20260101-abc"} {
		old := hubVersion
		hubVersion = func() string { return v }
		body := joinPost(t, onbUser(t), "1h")
		hubVersion = old
		mustHave(t, body, "data-join-manual")
		mustNotHave(t, body, `id="join-oneliner"`)
	}
	old := hubVersion
	hubVersion = func() string { return "v0.17.0-rc.1" }
	t.Cleanup(func() { hubVersion = old })
	body := joinPost(t, onbUser(t), "1h")
	mustHave(t, body, `id="join-oneliner"`)
	p := pres(body)
	for _, id := range []string{"join-oneliner", "join-service-install", "join-root-install", "join-compose-install", "join-windows-install", "join-docker", "join-compose-start"} {
		mustNotHave(t, p[id], "--accept-usage", "-AcceptUsage")
	}
}

// The request never waits for the lookup, runs at most one at a time, and a
// failure or a canceled request never erases a good result.
func TestInstallerLookupIsSingleFlightAndNeverBlocksOrDowngrades(t *testing.T) {
	newJoinFixture(t)
	old := hubVersion
	hubVersion = func() string { return "v1.2.3" }
	t.Cleanup(func() { hubVersion = old })
	release := make(chan struct{})
	var calls atomic.Int32
	t.Cleanup(SetInstallerSource(func(ctx context.Context, tag string) (version.InstallerInfo, error) {
		calls.Add(1)
		<-release
		return version.InstallerInfo{InstallerSHA256: testInstallerDigest, ChecksumsSHA256: testChecksumsDigest}, nil
	}))
	start := time.Now()
	for i := 0; i < 3; i++ {
		body := joinPost(t, onbUser(t), "1h")
		mustHave(t, body, `id="join-oneliner"`)
		mustNotHave(t, body, "data-join-verified")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the page waited for the lookup")
	}
	close(release)
	for i := 0; i < 200 && func() bool { _, ok := cachedInstaller(); return !ok }(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("lookups = %d, want 1", n)
	}
	mustHave(t, joinPost(t, onbUser(t), "1h"), `id="join-verified"`)
}

func TestInstallerFailureDoesNotEraseAGoodResult(t *testing.T) {
	newJoinFixture(t)
	withInstaller(t, testInstallerDigest)
	installerMu.Lock()
	installerState.failedAt = time.Now() // a later failure must not matter
	installerMu.Unlock()
	if _, ok := cachedInstaller(); !ok {
		t.Fatal("a good cached digest was lost")
	}
}

func TestSimpleOneLinerNeedsACompleteDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	dir := t.TempDir()
	curl := "#!/bin/sh\ncat \"$SERVED\"\nexit \"${CURL_STATUS:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	const script = "{ [ \"$MINION_JOIN\" = \"mnj1-test-token\" ] && [ \"$1\" = \"--service\" ] && [ \"$2\" = \"--accept-usage\" ] && touch \"$RAN\"; } # minion installer complete"
	for _, tt := range []struct {
		name, served, curlStatus string
		runs                     bool
	}{
		{"complete", script, "0", true},
		{"empty", "", "0", false},
		{"truncated", strings.Split(script, " #")[0], "0", false},
		{"failed transfer", script, "18", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dest := t.TempDir()
			scriptFile := filepath.Join(dest, "served")
			marker := filepath.Join(dest, "ran")
			if err := os.WriteFile(scriptFile, []byte(tt.served), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", installerModeLine("v1.2.3", "mnj1-test-token", "", "--service"))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "SERVED="+scriptFile, "RAN="+marker, "CURL_STATUS="+tt.curlStatus)
			out, err := cmd.CombinedOutput()
			_, markerErr := os.Stat(marker)
			if (err == nil) != tt.runs || (markerErr == nil) != tt.runs || strings.Contains(string(out), "mnj1-test-token") {
				t.Errorf("installer status = %v, marker = %v, output length = %d", err, markerErr, len(out))
			}
		})
	}
}
