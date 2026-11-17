package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func TestPairerWithExpectationStoresFingerprintCheck(t *testing.T) {
	hub, _ := identity.GenerateKey()
	f := newPairFixture(t, func(_ context.Context, p PairParams) (PairResult, error) {
		if !p.ExpectHub.Matches(hub.Public()) {
			t.Error("the expectation did not reach the dialer")
		}
		p.OnDial(transport.TLSSeen{Name: "hub.example", Issuer: "R11", Trusted: true, KeyHash: [32]byte{1}})
		p.OnWaiting()
		return PairResult{
			Pin:     identity.Pin{Pub: hub.Public()},
			Welcome: HubWelcome{Type: proto.TypeWelcome, WorkerID: "w-1", Name: p.Name, Owner: "alice", Kind: "private", HubTLS: &HubTLSInfo{Mode: "off"}},
			Version: proto.Current,
			TLS:     transport.TLSSeen{Name: "hub.example", Issuer: "R11", Trusted: true, KeyHash: [32]byte{1}},
		}, nil
	})
	r, _ := f.run(f.pairer(func(c *PairerConfig) { c.ExpectHub = mustMatcher(t, hub.Fingerprint().Words()) }))
	r.wait(t)
	if r.err != nil {
		t.Fatal(r.err)
	}
	enr, _, _ := f.enr.Load(context.Background())
	if enr.Check() != store.HubCheckFingerprint || enr.HubCheckAnchor != string(hub.Fingerprint()) {
		t.Fatalf("enrollment %+v", enr)
	}
	out := f.out.String()
	for _, want := range []string{
		"(expected; any other Hub is refused)", "TLS certificate:  certificate for hub.example, issued by",
		"It matched the fingerprint you gave.", "plain to Hub", hub.Fingerprint().Words(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("console lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(f.logs.String(), "hub_check=fingerprint") {
		t.Errorf("log:\n%s", f.logs.String())
	}
}

func TestPairerExpectationMismatchIsFatalAndStoresNothing(t *testing.T) {
	expected, _ := identity.GenerateKey()
	rogue, _ := identity.GenerateKey()
	f := newPairFixture(t, func(_ context.Context, p PairParams) (PairResult, error) {
		p.OnWaiting()
		return PairResult{}, &transport.HubKeyError{Presented: rogue.Public(), Err: transport.ErrExpectedHubMismatch}
	})
	r, _ := f.run(f.pairer(func(c *PairerConfig) { c.ExpectHub = mustMatcher(t, string(expected.Fingerprint())) }))
	r.wait(t)
	if !errors.Is(r.err, ErrHubFingerprintMismatch) {
		t.Fatalf("err = %v", r.err)
	}
	if f.dial.nCalls() != 1 {
		t.Fatalf("the pairer retried a refused Hub: %d calls", f.dial.nCalls())
	}
	if _, ok, _ := f.enr.Load(context.Background()); ok {
		t.Fatal("something was stored")
	}
	out := f.out.String()
	if strings.Count(out, "HUB FINGERPRINT DOES NOT MATCH - NOT PAIRED") != 1 ||
		!strings.Contains(out, expected.Fingerprint().Words()) || !strings.Contains(out, rogue.Fingerprint().Words()) {
		t.Fatalf("box M4:\n%s", out)
	}
	if !strings.Contains(f.logs.String(), "level=ERROR") {
		t.Fatalf("log:\n%s", f.logs.String())
	}
}

func TestTLSPathTable(t *testing.T) {
	seen := transport.TLSSeen{Name: "hub.example", Issuer: "R11", Trusted: true, KeyHash: [32]byte{7}}
	other := [32]byte{8}
	cases := []struct {
		seen   transport.TLSSeen
		stated *HubTLSInfo
		want   string
	}{
		{transport.TLSSeen{None: true}, &HubTLSInfo{Mode: "own", KeyHash: seen.KeyHash[:]}, TLSPathNone},
		{seen, nil, TLSPathUnknown},
		{seen, &HubTLSInfo{Mode: "own", KeyHash: seen.KeyHash[:]}, TLSPathKeysMatch},
		{seen, &HubTLSInfo{Mode: "own", KeyHash: other[:]}, TLSPathKeysDiffer},
		{seen, &HubTLSInfo{Mode: "off"}, TLSPathPlainToHub},
		{transport.TLSSeen{}, nil, TLSPathUnknown},
	}
	for _, c := range cases {
		got, line := TLSPath(c.seen, c.stated)
		if got != c.want || line == "" {
			t.Errorf("TLSPath(%+v, %+v) = %s %q, want %s", c.seen, c.stated, got, line, c.want)
		}
		if strings.Contains(line, "direct") {
			t.Errorf("the line must never say direct: %q", line)
		}
	}
	if f := TLSFacts(transport.TLSSeen{Name: "hub.lan", SelfSigned: true, Reason: "signed by an unknown authority", KeyHash: [32]byte{1}}); f != "certificate for hub.lan, self-signed, not trusted by this computer (signed by an unknown authority)" {
		t.Errorf("facts %q", f)
	}
}

// A v0.10-shaped welcome (no hub_tls) gives unknown; a welcome with hub_tls
// decodes into a struct that does not know the member.
func TestWelcomeHubTLSCompatibility(t *testing.T) {
	r, err := decodeReply([]byte(`{"type":"welcome","worker_id":"w-1","hub_tls":{"mode":"own","key_sha256":"AQ"}}`))
	if err != nil || r.Welcome.HubTLS == nil || r.Welcome.HubTLS.Mode != "own" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = decodeReply([]byte(`{"type":"welcome","worker_id":"w-1"}`))
	if err != nil || r.Welcome.HubTLS != nil {
		t.Fatalf("%+v %v", r, err)
	}
	if state, _ := TLSPath(transport.TLSSeen{KeyHash: [32]byte{1}}, r.Welcome.HubTLS); state != TLSPathUnknown {
		t.Fatal(state)
	}
}
