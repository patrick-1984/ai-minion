package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// setHubCheck stores check as the Hub check of the pairing in dir.
func setHubCheck(t *testing.T, dir, check, anchor string) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	e, _, err := agent.NewEnrollments(st, dir).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetHubCheck(context.Background(), e.Pin.Pub, check, anchor); err != nil {
		t.Fatal(err)
	}
}

// The line the Worker commands print first and hub_key.line of the status are
// the same bytes in every state (tray W10b), and the state is the Worker's
// own verdict.
func TestHubKeyLineByteEqual(t *testing.T) {
	cases := []struct {
		name, state, prefix string
		setup               func(t *testing.T, dir string)
	}{
		{"not paired", agent.HubKeyNone, "Hub key: none (this Worker is not paired)", func(*testing.T, string) {}},
		{"ok", agent.HubKeyOK, "Hub key: ", func(t *testing.T, dir string) {
			fp, _ := enrollWorker(t, dir)
			setHubCheck(t, dir, store.HubCheckFingerprint, fp)
		}},
		{"unverified", agent.HubKeyUnverified, "Hub key: ", func(t *testing.T, dir string) { enrollWorker(t, dir) }},
		{"mismatch", agent.HubKeyMismatch, "Hub key: ", func(t *testing.T, dir string) {
			fp, _ := enrollWorker(t, dir)
			setHubCheck(t, dir, store.HubCheckMismatch, fp)
		}},
		{"unreadable", agent.HubKeyUnknown, "Hub key: unknown (the Worker state cannot be read)", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, store.DBFile), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(t, dir)
			env := map[string]string{"MINION_STATE_DIR": dir}
			res := &config.Resolver{Role: config.ScopeWorker, LookupEnv: mapEnv(env)}
			line := hubIdentityLine(context.Background(), res)
			m := statusJSON(t, testStatusDeps(env))
			hk, _ := m["hub_key"].(map[string]any)
			if hk == nil || hk["line"] != line || hk["state"] != c.state || !strings.HasPrefix(line, c.prefix) {
				t.Fatalf("CLI line %q, status hub_key %v", line, m["hub_key"])
			}
			words, _ := hk["words"].(string)
			switch c.state {
			case agent.HubKeyNone, agent.HubKeyUnknown:
				if words != "" {
					t.Fatalf("words %q for %s", words, c.state)
				}
			default:
				if words == "" || words != m["hub_fingerprint_words"] || !strings.Contains(line, words) {
					t.Fatalf("words %q, status words %v, line %q", words, m["hub_fingerprint_words"], line)
				}
			}
			if (c.state == agent.HubKeyMismatch) != strings.Contains(line, "MISMATCH") {
				t.Fatalf("line %q for state %s", line, c.state)
			}
		})
	}
}

// The live alarm of a running Worker that refuses the Hub's key is a mismatch
// too, although the stored check is not.
func TestHubKeyAlarmIsMismatch(t *testing.T) {
	dir := t.TempDir()
	enrollWorker(t, dir)
	sd := testStatusDeps(nil)
	sd.live = func(string) agent.LiveSource {
		return fakeLive{ls: agent.LiveStatus{Connector: agent.Status{State: agent.ConnIdle, Alarm: agent.AlarmHubIdentityChanged}}}
	}
	m := statusJSON(t, sd, "--state-dir", dir)
	if hk := m["hub_key"].(map[string]any); hk["state"] != agent.HubKeyMismatch || !strings.Contains(hk["line"].(string), "MISMATCH") {
		t.Fatalf("hub_key = %v", hk)
	}
}
