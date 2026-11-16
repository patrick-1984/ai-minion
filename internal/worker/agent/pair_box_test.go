package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The pairing code and the Worker fingerprint stand in a box of ===== rules,
// and the fingerprint is exactly the one the Hub shows in its approve dialog.
func TestPairingShowsCodeAndFingerprintInABox(t *testing.T) {
	f := newPairFixture(t, func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		return PairResult{
			Pin: identity.Pin{Pub: make([]byte, 32)},
			Welcome: HubWelcome{Type: proto.TypeWelcome, WorkerID: "w-1", Name: p.Name, Owner: "alice",
				Kind: "private", Pending: true},
			Version: proto.Current,
		}, nil
	})
	r, _ := f.run(f.pairer())
	r.wait(t)
	if r.err != nil {
		t.Fatal(r.err)
	}
	key, err := f.enr.LoadKey()
	if err != nil {
		t.Fatal(err)
	}
	fpr := key.Fingerprint().String()
	out := f.out.String()
	for _, want := range []string{
		consoleRule + "\n  MINION WORKER PAIRING\n" + consoleRule + "\n",
		"  Pairing code: " + codeRE.FindString(out) + "\n",
		"  Worker fingerprint: " + fpr + "\n",
		consoleRule + "\n  MINION WORKER PAIRED\n" + consoleRule + "\n",
		"  paired to alice\n",
		"Waiting for approval",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("console lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "Worker fingerprint: "+fpr) != 2 {
		t.Errorf("the fingerprint must be in both boxes:\n%s", out)
	}
}

func TestConsoleBoxWritesNothingWithoutAConsole(t *testing.T) {
	writeConsoleBox(nil, "T", "x") // must not panic
}
