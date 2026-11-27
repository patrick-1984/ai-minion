package files

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// An API key revoked after the body, before the trailers: the commit gate
// asks Config.KeyLive, withholds the trailers and the record ends
// failed_not_sent (WP11: found end to end, the key principal is a snapshot).
func TestCommitGateRefusesARevokedAPIKey(t *testing.T) {
	var tok string
	var revoked atomic.Bool
	var asked atomic.Int32
	hs := newHarness(t, servePull(&tok, func() { revoked.Store(true) }), func(c *Config) {
		c.KeyLive = func(_ context.Context, id string) error {
			asked.Add(1)
			if id != keyID {
				t.Errorf("KeyLive asked for %q, want %q", id, keyID)
			}
			if revoked.Load() {
				return proto.NewError(proto.CodeForbidden, "not allowed")
			}
			return nil
		}
	})
	adm, d := hs.admit(ops.OpFilePull, ops.FilePullArgs{Path: "a.txt"}, fileAnswer())
	tok = targetToken(t, d)
	resp, _, err := get(t, hs.srv.URL+adm.LegPath, adm.LegToken)
	if err == nil && resp.Trailer.Get(proto.HeaderContentDigest) != "" {
		t.Fatal("the trailers were forwarded after the key was revoked")
	}
	r := waitState(t, hs.st, adm.TransferID, ops.FileStateFailedNotSent)
	if r.Reason != string(proto.CodeForbidden) || asked.Load() < 2 {
		t.Fatalf("reason %q, KeyLive asked %d times", r.Reason, asked.Load())
	}
}
