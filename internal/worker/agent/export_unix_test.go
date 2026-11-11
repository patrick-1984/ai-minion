//go:build unix

package agent

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// HubModeWorker is a running Worker in Hub mode for the external tests of
// other packages (the real CLI and the real MCP server drive its local
// socket): the whole module chain over a fake terminal host and in-memory Hub
// sessions. It exists only in test builds.
type HubModeWorker struct {
	// RunDir is the runtime directory of the local socket: set XDG_RUNTIME_DIR
	// and TMPDIR to it and the CLI finds the Worker.
	RunDir string
	// Socket is the path of the local socket.
	Socket string
	// Hub returns the Hub side of the Worker's next connection.
	Hub func() *transport.Session
	// WaitConnected returns once the call client has the Hub connection.
	WaitConnected func(t *testing.T)
}

// StartHubModeWorker starts an enrolled Worker with the setting
// call_api_key_file set to callKeyFile ("" for none).
func StartHubModeWorker(t *testing.T, callKeyFile string) *HubModeWorker {
	t.Helper()
	dir, _ := enrolledDir(t)
	ah, cls := newAnchorHost(t)
	w := startPipeRuntimeWithCallKey(t, dir, ah, cls, true, config.ShareExistingAll, callKeyFile)
	return &HubModeWorker{
		RunDir: w.runDir,
		Socket: w.sock,
		Hub:    func() *transport.Session { return <-w.dialer.hubs },
		WaitConnected: func(t *testing.T) {
			t.Helper()
			eventually(t, func() bool {
				cc, ok := Lookup[*CallClient](w.rt)
				return ok && cc.Connected()
			})
		},
	}
}
