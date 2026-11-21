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
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const testInstallerDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// withInstaller makes this Hub a release v1.2.3 whose signed manifest lists the
// installer digest sha.
func withInstaller(t *testing.T, sha string) {
	t.Helper()
	old := hubVersion
	hubVersion = func() string { return "v1.2.3" }
	t.Cleanup(func() { hubVersion = old })
	t.Cleanup(SetInstallerSource(func(_ context.Context, tag string) (string, error) {
		if tag != "v1.2.3" {
			return "", errors.New("unexpected tag " + tag)
		}
		return sha, nil
	}))
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
	body := joinPost(t, onbUser(t), "1h")
	tok, err := identity.NewJoinToken("https://hub.example.com:8443", f.key)
	if err != nil {
		t.Fatal(err)
	}
	p := pres(body)
	line := p["join-oneliner"]
	for _, want := range []string{
		"curl -fsSL https://github.com/patrick-1984/ai-minion/releases/download/v1.2.3/install.sh -o \"$f\"",
		`[ -s "$f" ]`, "grep -q '^" + testInstallerDigest + " '",
		"MINION_VERSION=v1.2.3", "MINION_JOIN=" + tok.Encode() + " sh \"$f\" --join; rm -f \"$f\"",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("one-liner lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("the one-liner is not one line:\n%s", line)
	}
	masked := p["join-masked"]
	if !strings.Contains(masked, testInstallerDigest) || !strings.Contains(masked, `sh "$f" --join; rm -f "$f"`) ||
		strings.Contains(masked, "MINION_JOIN") || strings.Contains(masked, tok.Encode()) || strings.Contains(masked, "\n") {
		t.Errorf("masked variant:\n%s", masked)
	}
	// It comes before the token field and the long variants.
	for _, later := range []string{`id="join-token"`, `id="join-unix"`, `id="join-docker"`} {
		if strings.Index(body, `id="join-oneliner"`) > strings.Index(body, later) {
			t.Errorf("the one-liner comes after %s", later)
		}
	}
	// No live region around the token.
	mustNotHave(t, body, `role="status" data-join-created`, `aria-live`)
}

func TestAddWorkerWithoutAKnownInstallerShowsTheManualCommands(t *testing.T) {
	newJoinFixture(t)
	old := hubVersion
	hubVersion = func() string { return "dev" }
	t.Cleanup(func() { hubVersion = old })
	t.Cleanup(SetInstallerSource(func(context.Context, string) (string, error) { return testInstallerDigest, nil }))
	body := joinPost(t, onbUser(t), "1h")
	mustHave(t, body, "data-join-manual", `id="join-unix"`, `id="join-docker"`)
	mustNotHave(t, body, `id="join-oneliner"`, `id="join-masked"`, testInstallerDigest)

	// A release whose installer cannot be verified is treated the same.
	withInstaller(t, "not-a-digest")
	mustHave(t, joinPost(t, onbUser(t), "1h"), "data-join-manual")
}

func TestAddWorkerUnlimitedKeyOnlyGetsTheMaskedCommand(t *testing.T) {
	f := newJoinFixture(t)
	f.ops.reply(ops.OpInvitationCreate, ops.InvitationCreateResult{ID: onbInviteID, Key: f.key.String(), Name: "n",
		Mode: ops.InvitationOneTime, Kind: ops.KindPrivate, TTL: ops.TTLUnlimited, CreatedAt: onbTime})
	withInstaller(t, testInstallerDigest)
	body := joinPost(t, onbAdmin(t), ops.TTLUnlimited)
	tok, _ := identity.NewJoinToken("https://hub.example.com:8443", f.key)
	mustHave(t, body, "data-join-unlimited", `id="join-masked"`, `id="join-token"`)
	mustNotHave(t, body, `id="join-oneliner"`, `id="join-unix"`, `id="join-system"`, `id="join-docker"`, `id="join-expanded"`, "MINION_JOIN=")
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
	offer := offerFor("v1.2.3", hex.EncodeToString(sum[:]))

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
		cmd := exec.Command("sh", "-c", installLine(offer, token))
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
	if strings.TrimSpace(string(argv)) != "--join" || strings.Contains(string(argv), "mnj1") ||
		strings.TrimSpace(string(join)) != tok.Encode() || strings.TrimSpace(string(ver)) != "v1.2.3" {
		t.Errorf("argv %q join %q ver %q", argv, join, ver)
	}
	for name, served := range map[string]string{"tampered": installer + "# evil\n", "truncated": installer[:20], "empty": ""} {
		if _, ran := run(served, tok.Encode()); ran {
			t.Errorf("%s download was run", name)
		}
	}
}
