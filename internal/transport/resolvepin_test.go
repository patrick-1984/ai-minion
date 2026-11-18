package transport

import (
	"bytes"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// resolvePin reports the keys the verified chain went through between the
// pin and the presented key, so that the Worker can keep the lineage.
func TestResolvePinReportsThePassedKeys(t *testing.T) {
	keys := make([]*identity.Key, 4)
	for i := range keys {
		k, err := identity.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = k
	}
	var chain []identity.Transition
	for i := 0; i+1 < len(keys); i++ {
		tr, err := identity.NewTransition(keys[i], keys[i+1].Public(), uint64(i+1), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, tr)
	}
	next, passed, err := resolvePin(identity.Pin{Pub: keys[0].Public()}, keys[3].Public(), chain)
	if err != nil || !bytes.Equal(next.Pub, keys[3].Public()) || len(passed) != 2 ||
		!bytes.Equal(passed[0], keys[1].Public()) || !bytes.Equal(passed[1], keys[2].Public()) {
		t.Fatalf("next %v, passed %d, err %v", next, len(passed), err)
	}
	// One link: nothing passed in between. No link: unchanged.
	if _, passed, err := resolvePin(identity.Pin{Pub: keys[2].Public(), Seq: 2}, keys[3].Public(), chain); err != nil || len(passed) != 0 {
		t.Fatalf("one link: %d %v", len(passed), err)
	}
	if _, passed, err := resolvePin(identity.Pin{Pub: keys[3].Public(), Seq: 3}, keys[3].Public(), chain); err != nil || len(passed) != 0 {
		t.Fatalf("no link: %d %v", len(passed), err)
	}
}
