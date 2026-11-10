package ops

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestPairingClaimIsAHubWriteOpOnTheCallersOwnSpace(t *testing.T) {
	op, ok := Lookup(OpPairingClaim)
	if !ok {
		t.Fatal("pairing_claim is not registered")
	}
	if op.Scope != ScopeHub || !op.Write || op.Stream {
		t.Errorf("op = %+v", op)
	}
	if op.Actions.String() != "operate" {
		t.Errorf("accepted actions = %s, want operate (a user's own space, not the admin-only manage)", op.Actions)
	}
	// A write op is POST only: the code must never travel in a URL.
	if got := strings.Join(op.RESTMethods(), ","); got != "POST" {
		t.Errorf("methods = %s, want POST only", got)
	}
	if op.RESTPath() != "/api/v1/ops/pairing_claim" {
		t.Errorf("path = %s", op.RESTPath())
	}
	var keep, warn int
	for _, g := range op.Guards {
		switch g.Class {
		case GuardKeep:
			keep++
		case GuardWarn:
			warn++
		default:
			t.Errorf("guard %s has class %s; a pairing code is never forced", g.Name, g.Class)
		}
	}
	if keep != 1 || warn != 1 {
		t.Errorf("guards = %+v", op.Guards)
	}
}

func TestPairingClaimArgsValidate(t *testing.T) {
	id := NewUUIDv7()
	ok := func(code, name string) PairingClaimArgs {
		return PairingClaimArgs{WriteArgs: WriteArgs{RequestID: id}, Code: code, Name: name}
	}
	tests := []struct {
		name    string
		a       PairingClaimArgs
		wantErr bool
	}{
		{"code only", ok("MN-K7Q4-9XTB", ""), false},
		{"lower case, spaces", ok("mn k7q4 9xtb", ""), false},
		{"with name", ok("MN-K7Q4-9XTB", "raspberry-pi"), false},
		{"no request id", PairingClaimArgs{Code: "MN-K7Q4-9XTB"}, true},
		{"no code", ok("", ""), true},
		{"code too long", ok(strings.Repeat("A", MaxPairingCodeLen+1), ""), true},
		{"code with control character", ok("MN-K7Q4\n9XTB", ""), true},
		{"code not UTF-8", ok("MN-\xff", ""), true},
		{"name with space", ok("MN-K7Q4-9XTB", "my pi"), true},
		{"name with colon", ok("MN-K7Q4-9XTB", "pi:1"), true},
		{"name too long", ok("MN-K7Q4-9XTB", strings.Repeat("n", MaxWorkerNameLen+1)), true},
		{"name at the limit", ok("MN-K7Q4-9XTB", strings.Repeat("n", MaxWorkerNameLen)), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.a.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("err %v does not match ErrInvalidArgument", err)
			}
			if strings.Contains(err.Error(), "K7Q4") {
				t.Errorf("the error repeats the code: %v", err)
			}
		})
	}
}

func TestPairingClaimArgsNeverPrintTheCode(t *testing.T) {
	a := PairingClaimArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, Code: "MN-K7Q4-9XTB", Name: "pi"}
	var sb strings.Builder
	log := slog.New(slog.NewTextHandler(&sb, nil))
	log.Info("call", "args", a, "ptr", &a)
	for name, s := range map[string]string{
		"%v": fmt.Sprintf("%v", a), "%+v": fmt.Sprintf("%+v", a), "%#v": fmt.Sprintf("%#v", a),
		"%v of a pointer": fmt.Sprintf("%v", &a), "slog": sb.String(),
	} {
		if strings.Contains(s, "9XTB") || strings.Contains(s, "K7Q4") {
			t.Errorf("%s prints the code: %s", name, s)
		}
		if !strings.Contains(s, "redacted") {
			t.Errorf("%s does not say the code is redacted: %s", name, s)
		}
	}
}

func TestValidWorkerNameIsTheTargetRule(t *testing.T) {
	for name, want := range map[string]bool{"pi": true, "build-box_2": true, "": false, "a b": false, "a:b": false, "tab\t": false} {
		if got := ValidWorkerName(name); got != want {
			t.Errorf("ValidWorkerName(%q) = %v, want %v", name, got, want)
		}
	}
}
