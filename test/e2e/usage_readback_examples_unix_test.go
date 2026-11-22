//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// guideBlock returns the first fenced block of lang after the heading in the
// local-integration guide.
func guideBlock(t *testing.T, heading, lang string) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/guides/integrate-local.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, heading)
	if i < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	s = s[i:]
	open := "```" + lang + "\n"
	j := strings.Index(s, open)
	if j < 0 {
		t.Fatalf("no %s block after %q", lang, heading)
	}
	s = s[j+len(open):]
	return s[:strings.Index(s, "\n```")+1]
}

// The examples of docs/guides/integrate-local.md run against the output of the
// real CLI and the files of a real, isolated Worker.
func TestGuideExamplesRunAgainstARealWorker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root Worker publishes in the public directory")
	}
	for _, tool := range []string{"jq", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	h := startShareHub(t)
	id, _ := (&shareE2E{t: t, ph: h}).user("guide-owner")
	c := startControllerWith(t, h, id, "guide-worker", "none", "", "s1")
	bin := minionBinary(t)
	home := t.TempDir()
	pathDir := t.TempDir()
	if err := os.Symlink(bin, filepath.Join(pathDir, "minion")); err != nil {
		t.Fatal(err)
	}
	env := append(c.env(""), "HOME="+home, "MINION_STATE_DIR="+c.stateDir)
	env = append(env, "PATH="+pathDir+":"+os.Getenv("PATH"))
	sh := func(script string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	var usageFile string
	eventually(t, "the Worker's presence files", func() bool {
		m, _ := filepath.Glob(filepath.Join(c.stateDir, "presence", "*.usage.json"))
		if len(m) == 1 {
			usageFile = m[0]
		}
		return len(m) == 1
	})
	instance := strings.TrimSuffix(filepath.Base(usageFile), ".usage.json")

	// The discovery document of the real CLI.
	var doc struct {
		Schema  string `json:"schema"`
		Workers []struct {
			Instance, Status, Source string
			DaemonVersion            string `json:"daemon_version"`
			OSScope                  string `json:"os_scope"`
			StaleAfterS              int    `json:"stale_after_s"`
			Usage                    json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(sh("minion worker discover --json --usage")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != "minion.worker-discovery/1" || len(doc.Workers) != 1 || doc.Workers[0].Instance != instance ||
		doc.Workers[0].Status != "running" || doc.Workers[0].OSScope != "user" || doc.Workers[0].StaleAfterS != 90 || len(doc.Workers[0].Usage) == 0 {
		t.Fatalf("discovery: %+v", doc)
	}
	version := doc.Workers[0].DaemonVersion
	if got := sh(guideBlock(t, "### Shell: is it running", "sh")); got != "minion "+version+" running (user)" {
		t.Fatalf("shell example: %q", got)
	}

	// Put a document with a headline where the Hub's would be, and draw the bar.
	now := time.Now().UnixMilli()
	meters := `{"schema":"minion.usage-meters/1","generated_at":` + itoa(now) + `,"slots":[{"slot":{"scope":"user","kind":"provider","label":"Claude Max"},"headline":"m1","meters":[{"id":"m1","metric":"anthropic.general.5h","label":"5 h","unit":"percent","used_percent":38,"remaining_percent":62,"observed_at":` + itoa(now-1000) + `,"expires_at":` + itoa(now+600000) + `,"coverage":{"known":1,"stale":0,"unknown":0}}]}]}`
	if err := os.WriteFile(usageFile, []byte(meters), 0o600); err != nil {
		t.Fatal(err)
	}
	bar := filepath.Join(pathDir, "minion-bar")
	if err := os.WriteFile(bar, []byte(guideBlock(t, "### tmux or status line", "sh")), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := sh("minion-bar"); got != "Claude Max [######----] 62%" {
		t.Fatalf("bar: %q", got)
	}

	// The Python reader over the real record: it reads <XDG_STATE_HOME>/minion/presence.
	xdg := t.TempDir()
	dst := filepath.Join(xdg, "minion", "presence")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	rec, err := os.ReadFile(filepath.Join(c.stateDir, "presence", instance+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, instance+".json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(t.TempDir(), "discover.py")
	if err := os.WriteFile(py, []byte(guideBlock(t, "### Python", "python")), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", py)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_STATE_HOME=" + xdg}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "running "+version+" (user)" {
		t.Fatalf("python example: %v %q", err, out)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
