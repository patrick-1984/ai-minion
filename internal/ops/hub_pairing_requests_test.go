package ops

import (
	"strings"
	"testing"
)

func TestPairingRequestOpsAreAdminOnlyHubOps(t *testing.T) {
	for name, write := range map[string]bool{OpPairingRequestsList: false, OpPairingRequestAdmit: true, OpPairingRequestReject: true} {
		op, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if op.Scope != ScopeHub || op.Write != write || op.Actions.String() != "manage" {
			t.Errorf("%s = scope %s write %v actions %s; want an admin-only (manage) hub op", name, op.Scope, op.Write, op.Actions)
		}
		for _, g := range op.Guards {
			if g.Name == guardPairingRequestOpen.Name && g.Class != GuardKeep {
				t.Errorf("%s: %s must be KEEP: force never admits another key", name, g.Name)
			}
		}
	}
}

func TestPairingRequestArgsValidate(t *testing.T) {
	fp := strings.Repeat("a", 52)
	ok := PairingRequestAdmitArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, PairingID: "K7q4", WorkerFingerprint: fp, Name: "pi"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]PairingRequestAdmitArgs{
		"no request id":   {PairingID: "K7Q4", WorkerFingerprint: fp},
		"short id":        {WriteArgs: ok.WriteArgs, PairingID: "K7Q", WorkerFingerprint: fp},
		"bad id":          {WriteArgs: ok.WriteArgs, PairingID: "K7-4", WorkerFingerprint: fp},
		"no fingerprint":  {WriteArgs: ok.WriteArgs, PairingID: "K7Q4"},
		"words not full":  {WriteArgs: ok.WriteArgs, PairingID: "K7Q4", WorkerFingerprint: "grid-duck-problem-valid-cloth-roof"},
		"name with colon": {WriteArgs: ok.WriteArgs, PairingID: "K7Q4", WorkerFingerprint: fp, Name: "a:b"},
	} {
		if err := a.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	rej := PairingRequestRejectArgs{WriteArgs: ok.WriteArgs, PairingID: "K7Q4", WorkerFingerprint: fp}
	if err := rej.Validate(); err != nil {
		t.Fatal(err)
	}
	rej.WorkerFingerprint = ""
	if rej.Validate() == nil {
		t.Error("a reject without the fingerprint was accepted")
	}
}
