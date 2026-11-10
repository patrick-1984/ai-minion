package ops

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestHubIdentityRotateIsAnAdminOnlyHubWrite(t *testing.T) {
	op, ok := Lookup(OpHubIdentityRotate)
	if !ok {
		t.Fatal("hub_identity_rotate is not registered")
	}
	if op.Scope != ScopeHub || !op.Write || op.Stream {
		t.Errorf("op = %+v", op)
	}
	if op.Actions.String() != "manage" {
		t.Errorf("accepted actions = %s, want manage only", op.Actions)
	}
	for _, a := range []Action{ActionRead, ActionOperate, ActionMessage} {
		if op.Actions.Permits(a) {
			t.Errorf("hub_identity_rotate must not accept %s", a)
		}
	}
	classes := map[string]GuardClass{}
	for _, g := range op.Guards {
		classes[g.Name] = g.Class
	}
	if classes["hub_identity_chain_not_full"] != GuardKeep || classes["hub_identity_invitations_revoked"] != GuardWarn {
		t.Errorf("guards = %v", classes)
	}
	if op.RESTPath() != "/api/v1/ops/hub_identity_rotate" {
		t.Errorf("path = %s", op.RESTPath())
	}
}

func TestHubIdentityRotateArgsNeedARequestID(t *testing.T) {
	if err := (HubIdentityRotateArgs{}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("no request id: err = %v", err)
	}
	var a HubIdentityRotateArgs
	if err := json.Unmarshal([]byte(`{"request_id":"`+NewUUIDv7()+`","force":false}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(); err != nil {
		t.Errorf("valid args: %v", err)
	}
}
