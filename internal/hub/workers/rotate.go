package workers

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// MaxChainLinks is the most links the Hub keeps in its transition chain. The
// whole chain travels in the Hub hello, a raw handshake frame of at most 64
// KiB (AD-6); a link is about 300 bytes of JSON, so this bound leaves a wide
// margin. Reaching it is a reason to re-enroll Workers on a fresh identity
// (the compromise path of AD-5) rather than to rotate again.
const MaxChainLinks = 128

// RotateAction is the audit action of a master-key rotation (hubid writes it
// in the transaction of the rotation).
const RotateAction = "hub_identity.rotate"

// ErrChainFull is the refusal of a rotation when the transition chain has
// [MaxChainLinks] links.
var ErrChainFull = fmt.Errorf("workers: the transition chain already has %d links and cannot grow; "+
	"replace the Hub identity and re-enroll the Workers instead", MaxChainLinks)

func transitionOf(r store.TransitionRow) identity.Transition {
	return identity.Transition{
		Seq: r.Seq, OldPub: identity.B64(bytes.Clone(r.OldPub)), NewPub: identity.B64(bytes.Clone(r.NewPub)),
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC(), SigByOld: identity.B64(bytes.Clone(r.SigByOld)),
	}
}

// ChainSource returns the function the connect [Handler] calls for the
// transition chain it sends in connect mode: the whole stored chain, oldest
// link first. The Hub does not know which key a connecting Worker is pinned
// to before the handshake finished, so it sends every link and the Worker
// follows the chain from its own pin: the handshake skips the links before
// the pinned key and verifies the rest (transport.WorkerHandshake). The chain
// is read per connection, so a rotation is visible at once.
func ChainSource(repo interface {
	Transitions(ctx context.Context) ([]store.TransitionRow, error)
}) func(ctx context.Context) ([]identity.Transition, error) {
	return func(ctx context.Context) ([]identity.Transition, error) {
		rows, err := repo.Transitions(ctx)
		if err != nil {
			return nil, err
		}
		chain := make([]identity.Transition, len(rows))
		for i, r := range rows {
			chain[i] = transitionOf(r)
		}
		return chain, nil
	}
}

// chainUpTo returns the links of chain that lead to the root key pub (the key
// of the certificate the connection presents; protocol 3.0 pins the root): a Worker applies the whole chain it receives
// and requires the result to be the key it meets, so a link whose new key the
// Hub does not use (yet) must not be sent. A chain that does not mention pub
// is returned unchanged, except that a pub equal to the first old key (no
// link applies) yields an empty chain.
func chainUpTo(chain []identity.Transition, pub []byte) []identity.Transition {
	for i, t := range chain {
		if bytes.Equal(t.NewPub, pub) {
			return chain[:i+1]
		}
	}
	if len(chain) > 0 && bytes.Equal(chain[0].OldPub, pub) {
		return nil
	}
	return chain
}
