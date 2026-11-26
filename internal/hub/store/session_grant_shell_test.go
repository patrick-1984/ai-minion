package store

import (
	"errors"
	"testing"
)

// A116: the command_shell flag is stored, changed with the revision, and only
// on a Control grant that names one target session.
func TestSessionGrantCommandShellFlag(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	row := f.ownerGrant("boss")
	row.Level, row.TargetSession, row.CommandShell = 3, "sh", true
	g := f.put(t, row)
	if !g.CommandShell || g.Revision != 1 {
		t.Fatalf("created = %+v", g)
	}
	got, err := f.s.SessionGrant(ctx, g.ID)
	if err != nil || !got.CommandShell {
		t.Fatalf("read back = %+v, %v", got, err)
	}
	if rows, err := f.s.SessionGrantsFor(ctx, f.a.ID, "boss"); err != nil || len(rows) != 1 || !rows[0].CommandShell {
		t.Fatalf("grants for boss = %+v, %v", rows, err)
	}
	// Clearing the flag is an update that moves the revision.
	got.CommandShell, got.UpdatedAt = false, 0
	off, err := f.s.PutSessionGrant(ctx, got, 1, GrantWrite{ActorID: "alice"})
	if err != nil || off.CommandShell || off.Revision != 2 {
		t.Fatalf("cleared = %+v, %v", off, err)
	}
	// The table refuses the flag below Control and on a whole-Worker grant.
	bad := off
	bad.CommandShell, bad.Level = true, 2
	if _, err := f.s.PutSessionGrant(ctx, bad, 2, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("flag on a Message grant: %v", err)
	}
	wide := f.ownerGrant("other")
	wide.Level, wide.CommandShell = 3, true
	if _, err := f.s.PutSessionGrant(ctx, wide, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("flag on a whole-Worker grant: %v", err)
	}
	// Only the authority of the grant may set it: bob owns neither Worker.
	on := off
	on.CommandShell = true
	if _, err := f.s.PutSessionGrant(ctx, on, 2, GrantWrite{ActorID: "bob"}); !errors.Is(err, ErrGrantAuthority) {
		t.Fatalf("flag set by a stranger: %v", err)
	}
}
