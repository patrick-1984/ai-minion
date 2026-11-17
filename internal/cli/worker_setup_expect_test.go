package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Without hub_fingerprint the wizard asks for it after the address was typed
// and before the address is checked, saves it with the address and pairs with
// the expectation.
func TestWorkerSetupAsksForTheHubFingerprintBeforeTheCheck(t *testing.T) {
	fp := fakeHubFingerprint(t)
	for name, answer := range map[string]string{"words": fp.Words(), "skip": "skip"} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, true, "build")
			delete(r.env, "MINION_HUB_FINGERPRINT")
			k := newKeys(t)
			k.typeLines("1", "https://hub.example:8443", "", "nonsense", answer)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := r.run(ctx, k.r); err != nil {
				t.Fatalf("worker setup: %v\n%s", err, r.stdout)
			}
			out := r.stdout.String()
			ask, check := strings.Index(out, "NEW HUB - ENTER ITS FINGERPRINT"), strings.Index(out, "Checking https://hub.example:8443")
			if ask < 0 || check < 0 || ask > check {
				t.Fatalf("the fingerprint must be asked before the check:\n%s", out)
			}
			if !strings.Contains(out, "That is not a fingerprint") {
				t.Errorf("an invalid answer was not explained:\n%s", out)
			}
			e, ok := r.enrollment()
			if !ok {
				t.Fatal("not paired")
			}
			saved := r.file()[config.KeyHubFingerprint]
			if name == "skip" {
				if saved != "" || e.Check() != store.HubCheckNone {
					t.Fatalf("skip: saved %q, check %q", saved, e.Check())
				}
				return
			}
			if saved != fp.Words() || e.Check() != store.HubCheckFingerprint {
				t.Fatalf("saved %q, check %q", saved, e.Check())
			}
			if !r.pair.calls[0].ExpectHub.MatchesFingerprint(fp) {
				t.Fatal("the pairing did not get the expectation")
			}
		})
	}
}
