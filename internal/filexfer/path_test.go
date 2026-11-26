package filexfer

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// FR-5: hostile paths are path_outside_root on every platform.
func TestCheckPathHostile(t *testing.T) {
	decoded, _ := url.PathUnescape("%2e%2e/x")
	hostile := []string{
		"../x", "/etc/passwd", "a/../../b", `C:\x`, `a\..\b`, "a/./b", decoded, "a//b", "a/", ".",
		strings.Repeat("x", 256), "a\x00b", "a\nb", "\x7f", "a/\u0085", strings.Repeat("a/", 32) + "b",
		strings.Repeat("abcdefgh/", 600), ".minion-part-x", "d/.minion-part-1", ".minion/manifest.json",
		"e\u0301", // NFD: e + combining acute
		"\xff",
	}
	for _, p := range hostile {
		err := CheckPath(p, false)
		if proto.CodeOf(err) != proto.CodePathOutsideRoot {
			t.Fatalf("%q: got %v", p, err)
		}
		if err != nil && p != "" && len(p) > 3 && strings.Contains(err.Error(), p) {
			t.Fatalf("%q: the message names the path", p)
		}
	}
	for _, p := range []string{"a", "a/b.txt", ".git/HEAD", "Makefile", "\u00e9", strings.Repeat("x", 255)} {
		if err := CheckPath(p, false); err != nil {
			t.Fatalf("%q refused: %v", p, err)
		}
	}
	if CheckPath("", true) != nil || CheckPath("", false) == nil {
		t.Fatal("empty path rules")
	}
}

func TestProtectedAndSensitiveNames(t *testing.T) {
	protected := []string{".git/hooks/pre-commit", ".git/hooks", ".git/config", ".claude/settings.json", ".envrc",
		".env.local", "sub/.bashrc", ".github/workflows/ci.yml", ".vscode/settings.json", ".ssh/authorized_keys",
		"a/.Git/Hooks/x", ".mcp.json", "x/.config/fish/config.fish", ".hg/hgrc", ".gitlab-ci.yml"}
	for _, p := range protected {
		if !PushProtected(p) {
			t.Fatalf("%s not push-protected", p)
		}
	}
	for _, p := range []string{"Makefile", ".git/HEAD", ".git/objects/ab/cd", ".github/README.md", ".vscode/launch.json", "src/env.go"} {
		if PushProtected(p) {
			t.Fatalf("%s push-protected", p)
		}
	}
	sensitive := []string{".ssh/id_ed25519", "id_rsa", "keys/server.pem", "tls.key", ".aws/credentials", ".netrc", ".git-credentials", "a/.gnupg/x"}
	for _, p := range sensitive {
		if !PullSensitive(p) {
			t.Fatalf("%s not sensitive", p)
		}
	}
	for _, p := range []string{".git/HEAD", "id_rsa.pub", "keyboard.txt", "src/main.go"} {
		if PullSensitive(p) {
			t.Fatalf("%s sensitive", p)
		}
	}
}

func TestLimitsNormalize(t *testing.T) {
	l := Limits{MaxBytes: 10, MaxEntries: 1 << 40}.Normalize()
	if l.MaxBytes != 10 || l.MaxEntries != proto.MaxTransferEntries || l.MaxDepth != proto.MaxPathDepth {
		t.Fatalf("%+v", l)
	}
	if err := l.checkPathLimits(strings.Repeat("a/", 32) + "a"); proto.CodeOf(err) != proto.CodeTransferTooLarge {
		t.Fatalf("depth: %v", err)
	}
}
