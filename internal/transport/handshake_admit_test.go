package transport

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// admitConfigs is a pair_code handshake whose Hub admits the request (no code
// is ever claimed) and whose Worker announces and allows pair.admit.
func admitConfigs(t *testing.T) (WorkerConfig, HubConfig) {
	t.Helper()
	wc, hc, _ := pairConfigs(t)
	wc.AllowAdmit = true
	wc.Features = []string{proto.FeaturePairAdmit}
	hc.PairingPassword = nil
	hc.PairingWait = func(string) (PairingAnswer, error) { return PairingAnswer{Admit: true}, nil }
	return wc, hc
}

func TestAdmittedPairingRequestCompletesWithoutCPace(t *testing.T) {
	wc, hc := admitConfigs(t)
	var shown ed25519.PublicKey
	wc.OnHubHello = func(root ed25519.PublicKey) { shown = root }
	o := runPair(t, wc, hc, nil)
	o.mustSucceed(t)
	if !o.worker.Admitted || !o.hub.Admitted {
		t.Fatalf("admitted: worker %v, hub %v", o.worker.Admitted, o.hub.Admitted)
	}
	if !bytes.Equal(shown, hc.Cert.RootPub) || !bytes.Equal(o.worker.Pin.Pub, hc.Cert.RootPub) {
		t.Error("the Worker did not show and pin the root of the Hub hello")
	}
	if !bytes.Equal(o.hub.WorkerPub, wc.Key.Public()) {
		t.Error("the Hub did not authenticate the presented Worker key")
	}
	// The Worker never sends its CPace confirmation: nothing about the code is
	// exposed by an admitted request.
	if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello,pake.msg,sig" {
		t.Errorf("worker frames %s", got)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "hello,pair.admit,sig" {
		t.Errorf("hub frames %s", got)
	}
	o.roundTrip(t)
}

func TestAWorkerThatDoesNotAllowAdmitRefusesPairAdmit(t *testing.T) {
	wc, hc := admitConfigs(t)
	wc.AllowAdmit = false // it still announces the feature: the Worker's own rule decides
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrProtocol) {
		t.Fatalf("worker error %v, want a protocol error", o.workerErr)
	}
	if got := strings.Join(frameTypes(t, o.wWire), ","); got != "hello,pake.msg" {
		t.Errorf("worker frames %s: nothing may follow a refused admission", got)
	}
}

func TestTheHubAdmitsOnlyAWorkerThatAnnouncedTheFeature(t *testing.T) {
	wc, hc := admitConfigs(t)
	wc.Features = nil
	o := runPair(t, wc, hc, nil)
	if proto.CodeOf(o.hubErr) != proto.CodeInvalidArgument {
		t.Fatalf("hub error %v", o.hubErr)
	}
	if got := strings.Join(frameTypes(t, o.hWire), ","); got != "hello,error" {
		t.Errorf("hub frames %s", got)
	}
}

// A39: the CPace session id is the transcript hash, which covers the Hub
// hello with its root key and certificate. A relay that shows the Worker
// another root (its own Hub, with a valid certificate under that root) while
// the code is entered in the real Hub cannot complete CPace: the Worker ends
// with a failed confirmation and sends no sig_worker. So a code entered in the
// real Hub authenticates exactly the root the Worker showed.
func TestTheCodeAuthenticatesTheRootTheWorkerShowed(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	other := newHub(t)
	var shown ed25519.PublicKey
	wc.OnHubHello = func(root ed25519.PublicKey) { shown = root }
	o := runPair(t, wc, hc, func(fromWorker bool, idx int, body []byte) []byte {
		if fromWorker || idx != 0 {
			return body
		}
		return mutateHubHello(t, body, func(m map[string]json.RawMessage) {
			m["hub_pub"] = rawB64(t, other.root.Public())
			m["hub_cert"] = rawB64(t, other.cert.Bytes())
		})
	})
	if !bytes.Equal(shown, other.root.Public()) {
		t.Fatal("the Worker did not show the root it received")
	}
	if o.worker != nil || o.workerErr == nil {
		t.Fatal("the Worker paired with a substituted root")
	}
	for _, f := range frameTypes(t, o.wWire) {
		if f == proto.TypeSig {
			t.Fatal("the Worker sent sig_worker to a substituted root")
		}
	}
}

func TestOnHubHelloIsNotCalledForARefusedExpectation(t *testing.T) {
	wc, hc, _ := pairConfigs(t)
	fp, err := identity.FingerprintOf(newHub(t).root.Public())
	if err != nil {
		t.Fatal(err)
	}
	wc.ExpectHub = matcherFor(t, fp, false)
	called := false
	wc.OnHubHello = func(ed25519.PublicKey) { called = true }
	o := runPair(t, wc, hc, nil)
	if !errors.Is(o.workerErr, ErrExpectedHubMismatch) || called {
		t.Fatalf("worker error %v, called %v", o.workerErr, called)
	}
}

func TestARejectedRequestKeepsOnlyTheReasonToken(t *testing.T) {
	wc, hc := admitConfigs(t)
	hc.PairingWait = func(string) (PairingAnswer, error) {
		e := proto.NewError(proto.CodePairingCodeInvalid, "free text of the peer")
		e.Details = []byte(`{"reason":"rejected","other":"x"}`)
		return PairingAnswer{}, e
	}
	o := runPair(t, wc, hc, nil)
	if PeerReason(o.workerErr) != "rejected" {
		t.Fatalf("worker error %v, reason %q", o.workerErr, PeerReason(o.workerErr))
	}
	if strings.Contains(o.workerErr.Error(), "free text") {
		t.Error("the peer's wording was shown")
	}
	if PeerReason(proto.NewError(proto.CodePairingCodeInvalid, "x")) != "" {
		t.Error("a reason without details")
	}
}
