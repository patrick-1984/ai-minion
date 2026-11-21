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

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestUsageRegisteredWorkerAutomaticallyReportsUnsharedSessionAndNativeQuota(t *testing.T) {
	h := startShareHub(t)
	id, headers := (&shareE2E{t: t, ph: h}).user("metrics-owner")
	bin := t.TempDir()
	for _, harness := range []string{"claude", "codex", "gemini", "opencode", "aider", "qwen", "cline"} {
		script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 1.2.3; exit 0; fi\n"
		if harness == "codex" {
			script += `if [ "$1" != app-server ]; then exit 1; fi
while IFS= read -r line; do
 case "$line" in
  *'"method":"initialize"'*) echo '{"id":1,"result":{}}';;
  *'"method":"account/read"'*) echo '{"id":2,"result":{"account":{"type":"chatgpt","email":"dev@example.invalid","planType":"pro"}}}';;
  *'"method":"account/rateLimits/read"'*) echo '{"id":3,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":17,"windowDurationMins":300}}}}';;
 esac
done
`
		} else {
			script += "exit 1\n"
		}
		if err := os.WriteFile(filepath.Join(bin, harness), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := startControllerWith(t, h, id, "usage-worker", "none", bin, "private-build")
	get := func(section string) ops.UsageListResult {
		t.Helper()
		code, body := h.do("POST", "/api/v1/ops/"+ops.OpUsageList, map[string]any{"section": section, "worker_id": c.wid}, headers)
		var result ops.UsageListResult
		if code != 200 || json.Unmarshal([]byte(body), &result) != nil {
			t.Fatalf("%s: HTTP %d: %s", section, code, body)
		}
		return result
	}
	// Wait for the real scheduler, without invoking the manual scan endpoint.
	deadline := time.Now().Add(25 * time.Second)
	for {
		sessions, accounts := get("sessions"), get("accounts")
		if len(sessions.Sessions) == 1 && sessions.Sessions[0].Name == "private-build" && len(accounts.Accounts) == 1 && len(accounts.Accounts[0].Windows) == 1 {
			w := accounts.Accounts[0].Windows[0]
			if w.UsedPercent == nil || *w.UsedPercent != 17 || accounts.Accounts[0].Plan != "pro" {
				t.Fatalf("native source: %+v", accounts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic collection did not finish: workers=%+v sessions=%+v accounts=%+v", get("workers"), sessions, accounts)
		}
		time.Sleep(100 * time.Millisecond)
	}
	code, body := h.do("POST", "/api/v1/ops/"+ops.OpSessionsList, map[string]any{"worker_id": c.wid}, headers)
	if code != 200 || strings.Contains(body, "private-build") {
		t.Fatalf("ordinary session ACL: HTTP %d: %s", code, body)
	}
	cmd := exec.Command(minionBinary(t), "worker", "config", "usage", "record", "gemini-result", "--state-dir", c.stateDir, "--session", "private-build", "--native-session", "fixture")
	cmd.Env = c.env("")
	cmd.Stdin = strings.NewReader(`{"response":"PRIVATE-CONTENT","stats":{"models":{"model-fixture":{"tokens":{"prompt":14,"candidates":4}}}}}`)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native exporter: %v: %s", err, b)
	}
	eventually(t, "native counters on registered private session", func() bool {
		r := get("sessions")
		return len(r.Sessions) == 1 && r.Sessions[0].InputTokens != nil && *r.Sessions[0].InputTokens == 14 && r.Sessions[0].Folder != ""
	})
}
