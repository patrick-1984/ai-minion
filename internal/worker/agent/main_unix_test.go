//go:build unix

package agent

import (
	"os"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/testutil/fakeagent"
)

// TestMain lets the test binary be the fake agent of a tmux pane (see
// internal/testutil/fakeagent), as the pipe integration test needs.
func TestMain(m *testing.M) {
	if fakeagent.Run() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}
