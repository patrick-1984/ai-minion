package transport

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func matcherFor(t *testing.T, fp identity.Fingerprint, words bool) identity.FingerprintMatcher {
	t.Helper()
	in := string(fp)
	if words {
		in = fp.Words()
	}
	m, err := identity.ParseFingerprintInput(in)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPairCodeExpectHubMismatchSendsNoPakeMessage(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	other := mustKey(t)
	wc.ExpectHub = matcherFor(t, other.Fingerprint(), true)
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrExpectedHubMismatch) {
		t.Fatalf("worker error %v, want ErrExpectedHubMismatch", o.workerErr)
	}
	var hk *HubKeyError
	if !errors.As(o.workerErr, &hk) || !bytes.Equal(hk.Presented, hc.Cert.RootPub) {
		t.Fatalf("the error must carry the presented root: %#v", o.workerErr)
	}
	if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello" {
		t.Errorf("worker sent %s, want only its hello (no pake.msg)", got)
	}
	if ErrorCode(o.workerErr) != proto.CodeUnauthenticated {
		t.Errorf("code %s", ErrorCode(o.workerErr))
	}
}

func TestPairCodeExpectHubMatchPasses(t *testing.T) {
	for _, words := range []bool{false, true} {
		wc, hc, _ := pairConfigs(t)
		fp, err := identity.FingerprintOf(hc.Cert.RootPub)
		if err != nil {
			t.Fatal(err)
		}
		wc.ExpectHub = matcherFor(t, fp, words)
		o := runPair(t, wc, hc, nil)
		o.mustSucceed(t)
		o.roundTrip(t)
	}
}

func TestEnrollInviteExpectHub(t *testing.T) {
	wc, hc, _ := enrollConfigs(t)
	wc.ExpectHub = matcherFor(t, mustKey(t).Fingerprint(), false)
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrExpectedHubMismatch) {
		t.Fatalf("worker error %v", o.workerErr)
	}
	if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello" {
		t.Errorf("worker sent %s", got)
	}
	wc, hc, hk := enrollConfigs(t)
	wc.ExpectHub = matcherFor(t, hk.Fingerprint(), true)
	o = runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	o.roundTrip(t)
}

func TestConnectPinErrorsCarryThePresentedRoot(t *testing.T) {
	wc, hc, _, hk := connectConfigs(t)
	wc.Pin = identity.Pin{Pub: mustKey(t).Public()}
	o := runPair(t, wc, hc, nil)
	var hke *HubKeyError
	if !errors.Is(o.workerErr, ErrPinMismatch) || !errors.As(o.workerErr, &hke) || !bytes.Equal(hke.Presented, hk.Public()) {
		t.Fatalf("pin mismatch: %v", o.workerErr)
	}

	// An invalid chain: a link with a bad signature.
	keys, chain, hubs := rotate(t, 2)
	forged := bytes.Clone(chain[1].SigByOld)
	forged[0] ^= 1
	bad := []identity.Transition{chain[0], {Seq: 2, OldPub: chain[1].OldPub, NewPub: chain[1].NewPub, CreatedAt: chain[1].CreatedAt, SigByOld: forged}}
	wc, _, _, _ = connectConfigs(t)
	wc.Pin = identity.Pin{Pub: keys[0].Public()}
	hc = hubs[2].cfg()
	hc.Chain = bad
	o = runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrChainInvalid) || !errors.As(o.workerErr, &hke) || !bytes.Equal(hke.Presented, keys[2].Public()) {
		t.Fatalf("chain error must carry the presented root: %v", o.workerErr)
	}
}
