package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// Gate-1 C: usage failures of a command run with --json are one result object
// and exit 2.
func TestUsageFailuresAreJSONResults(t *testing.T) {
	for _, args := range [][]string{
		{"docs", "bogus", "--json"},
		{"worker", "status", "extra", "--json"},
		{"worker", "share", "add", "--json"},
		{"worker", "restart", "--json", "--accept-loss", "invalid"},
		{"worker", "status", "--json", "--no-such-flag"},
		{"worker", "join", "a", "b", "--resume", "--json"},
	} {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), append([]string{"minion"}, args...), &out, &errOut)
		var m map[string]any
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Errorf("%v: stdout %q (%v)", args, out.String(), err)
			continue
		}
		if code != ExitUsage || m["ok"] != false || m["code"] != "usage" {
			t.Errorf("%v: code %d, %v", args, code, m)
		}
	}
	// Without --json nothing changes: the plain error goes to stderr.
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"minion", "docs", "bogus"}, &out, &errOut); code != ExitUsage || out.Len() != 0 {
		t.Errorf("no --json: %d %q", code, out.String())
	}
}
