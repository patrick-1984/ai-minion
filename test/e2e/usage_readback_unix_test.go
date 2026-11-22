//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Nominate on the Hub and the real Worker's private presence directory gets the
// document (mode 0600, the Hub's bytes, minion.usage-meters/1); withdraw and it
// is replaced by an empty one within the deadline.
func TestUsageReadbackNominationReachesTheWorkersFileAndWithdrawalUpdatesIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root Worker publishes in the public directory; this test reads the private one")
	}
	h := startShareHub(t)
	id, headers := (&shareE2E{t: t, ph: h}).user("readback-owner")
	c := startControllerWith(t, h, id, "readback-worker", "none", "", "s1")
	t.Cleanup(func() {
		if t.Failed() {
			_ = filepath.WalkDir(c.stateDir, func(p string, e fs.DirEntry, err error) error {
				t.Logf("state: %s", strings.TrimPrefix(p, c.stateDir))
				return nil
			})
		}
	})
	call := func(op string, args any) string {
		t.Helper()
		code, body := h.do("POST", "/api/v1/ops/"+op, args, headers)
		if code != 200 {
			t.Fatalf("%s: HTTP %d: %s", op, code, body)
		}
		return body
	}
	usageFile := func() (string, []byte) {
		var path string
		_ = filepath.WalkDir(filepath.Join(c.stateDir, "presence"), func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(p, ".usage.json") {
				path = p
			}
			return nil
		})
		if path == "" {
			return "", nil
		}
		b, _ := os.ReadFile(path)
		return path, b
	}
	slots := func() (int, string) {
		_, b := usageFile()
		if b == nil {
			return -1, ""
		}
		var doc struct {
			Schema string `json:"schema"`
			Slots  []struct {
				Slot struct {
					Scope, Kind, Label string
				} `json:"slot"`
			} `json:"slots"`
		}
		if json.Unmarshal(b, &doc) != nil || doc.Schema != ops.UsageMetersSchema {
			return -1, ""
		}
		if len(doc.Slots) == 0 {
			return 0, ""
		}
		return len(doc.Slots), doc.Slots[0].Slot.Label
	}

	// Admission delivers the (empty) document at once.
	eventually(t, "the first document on the Worker", func() bool { n, _ := slots(); return n == 0 })
	path, _ := usageFile()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the user-mode file is not private: %v %v", fi, err)
	}

	var bucket ops.UsageBucketSetResult
	if err := json.Unmarshal([]byte(call(ops.OpUsageBucketSet, ops.UsageBucketSetArgs{Action: "save", Kind: "session", Label: "Team"})), &bucket); err != nil || bucket.ID == "" {
		t.Fatalf("bucket: %+v %v", bucket, err)
	}
	call(ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: "nominate", Scope: "user", Kind: "session", BucketID: bucket.ID})
	start := time.Now()
	eventually(t, "the nominated slot in the Worker's file", func() bool { n, l := slots(); return n == 1 && l == "Team" })
	t.Logf("nomination reached the file in %v", time.Since(start))

	call(ops.OpUsageNominationSet, ops.UsageNominationSetArgs{Action: "clear", Scope: "user", Kind: "session"})
	start = time.Now()
	eventually(t, "the withdrawal in the Worker's file", func() bool { n, _ := slots(); return n == 0 })
	t.Logf("withdrawal reached the file in %v", time.Since(start))
}
