package hubid

import (
	"context"
	"testing"
)

// RC2 release-gate reproducer (H3 bootstrap): between the commit of the first
// root and its durability confirmation, no invitation may name that root.
func TestInvitationRootRefusesTheFirstRootBeforeItIsConfirmedDurable(t *testing.T) {
	e := newEnv(t)
	s := e.svc()
	var (
		atCommit, atConfirm       string
		atCommitErr, atConfirmErr error
		inConfirm                 bool
	)
	e.point = func(n string) error {
		if n == PointBSCommitted {
			atCommit, atCommitErr = s.InvitationRoot(context.Background())
			inConfirm = true
		}
		return nil
	}
	e.makeDurable = func(context.Context) error {
		if inConfirm {
			inConfirm = false
			atConfirm, atConfirmErr = s.InvitationRoot(context.Background())
		}
		return nil
	}
	if _, err := s.SetPassphrase(bg, ui, pw1, pw1, IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, atCommitErr, ErrConflict)
	wantCode(t, atConfirmErr, ErrConflict)
	if atCommit != "" || atConfirm != "" {
		t.Fatalf("an unconfirmed first root was offered: %q %q", atCommit, atConfirm)
	}
	// Once confirmed and serving, the root is offered.
	got, err := s.InvitationRoot(context.Background())
	if err != nil || got != e.row().Fingerprint {
		t.Fatalf("after confirmation: %q %v", got, err)
	}
}
