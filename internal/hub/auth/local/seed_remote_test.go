package local

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/logx"
)

// TestGeneratedPasswordNeverReachesARemoteSink: the generated admin password
// is a human notice. The console gets it at error level as before; a remote
// sink built with logx.RemoteSafe gets nothing of it.
func TestGeneratedPasswordNeverReachesARemoteSink(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	local := &syncBuf{}
	var remote bytes.Buffer
	log := slog.New(logx.Tee(
		slog.NewTextHandler(local, &slog.HandlerOptions{Level: slog.LevelError}),
		logx.RemoteSafe(slog.NewJSONHandler(&remote, &slog.HandlerOptions{Level: slog.LevelDebug})),
	))
	sd := &Seeder{Store: st, Hasher: fastHasher(), Log: log}
	res, err := sd.Seed(ctx, SeedConfig{})
	if err != nil || !res.Generated {
		t.Fatalf("Seed = %+v, %v", res, err)
	}
	var pw string
	for _, f := range strings.Fields(local.String()) {
		if v, ok := strings.CutPrefix(f, "password="); ok {
			pw = v
		}
	}
	if len(pw) != 24 || !strings.Contains(local.String(), "level=ERROR") {
		t.Fatalf("the console lost the error-level notice:\n%s", local)
	}
	if strings.Contains(remote.String(), pw) || strings.Contains(remote.String(), "generated password") {
		t.Fatalf("the remote sink received the notice:\n%s", remote.String())
	}
}
