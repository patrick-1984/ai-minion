package authz_test

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
)

// TestCommandShellGrant (A116): only a valid Control grant with the flag, on
// exactly the named session, marks a command shell; the flag grants nothing.
func TestCommandShellGrant(t *testing.T) {
	a := dgAuthz(grantStore())
	ctx := context.Background()
	shell := func(id, session string, level int) authz.DirectGrant {
		g := ownerGrant(id, "W2", session, level)
		g.CommandShell = true
		return g
	}
	expired := shell("gx", "sh", 3)
	expired.ExpiresAt = authz.ExpiryAt(dgNow.Add(-time.Second))
	suspended := shell("gs", "sh", 3)
	suspended.Suspended = true
	foreign := shell("gf", "sh", 3)
	foreign.TargetOwnerID = "bob" // the target changed owner since
	for _, c := range []struct {
		name   string
		grants []authz.DirectGrant
		res    authz.Resource
		want   string
	}{
		{"control with flag", []authz.DirectGrant{shell("g1", "sh", 3)}, sess("W2", "sh"), "g1"},
		{"other session", []authz.DirectGrant{shell("g1", "sh", 3)}, sess("W2", "other"), ""},
		{"no flag", []authz.DirectGrant{ownerGrant("g1", "W2", "sh", 3)}, sess("W2", "sh"), ""},
		{"message level", []authz.DirectGrant{shell("g1", "sh", 2)}, sess("W2", "sh"), ""},
		{"whole Worker", []authz.DirectGrant{shell("g1", "", 3)}, sess("W2", "sh"), ""},
		{"expired", []authz.DirectGrant{expired}, sess("W2", "sh"), ""},
		{"suspended", []authz.DirectGrant{suspended}, sess("W2", "sh"), ""},
		{"owner changed", []authz.DirectGrant{foreign}, sess("W2", "sh"), ""},
		{"worker resource", []authz.DirectGrant{shell("g1", "sh", 3)}, authz.WorkerResource{ID: "W2"}, ""},
		{"second grant has the flag", []authz.DirectGrant{ownerGrant("g0", "W2", "", 3), shell("g1", "sh", 3)}, sess("W2", "sh"), "g1"},
	} {
		p := authz.NewWorkerSessionPrincipal("A1", "main", nil, c.grants)
		got, err := a.CommandShellGrant(ctx, p, c.res)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	// A principal that is no Worker session never has one.
	if got, err := a.CommandShellGrant(ctx, authz.NewWorkerGrantPrincipal("A1", nil), sess("W2", "sh")); err != nil || got != "" {
		t.Errorf("plain Worker: %q %v", got, err)
	}
}
