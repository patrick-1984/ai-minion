package store

import (
	"context"
	"testing"
)

// The revision of the share selection only rises: every committed change raises
// it by one, in the transaction of the change; reading never does; a pairing
// reset that removes the choice counts as a change, and the revision is not
// reset with the choice. It survives a reopen. The Hub orders the rules of a
// Worker by it.
func TestShareRevisionOnlyRises(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	ctx := context.Background()

	st, err := s.LoadShareStateContext(ctx)
	if err != nil || st.Revision != 1 || st.Configured || st.Mode != ShareLater {
		t.Fatalf("a new database: %+v %v, want revision 1, not configured, later", st, err)
	}
	if again, _ := s.LoadShareStateContext(ctx); again.Revision != 1 {
		t.Fatal("reading raised the revision")
	}

	got, err := s.CommitShareContext(ctx, ShareSelection{Mode: ShareSelect, Sessions: []string{"b", "a", "b"}})
	if err != nil || got.Revision != 2 || got.Mode != ShareSelect || len(got.Sessions) != 2 || !got.Configured {
		t.Fatalf("commit = %+v %v", got, err)
	}
	if st, _ := s.LoadShareStateContext(ctx); st.Revision != got.Revision {
		t.Fatalf("loaded revision %d, committed %d: the commit must report what it stored", st.Revision, got.Revision)
	}
	if err := s.SaveShare(ShareSelection{Mode: ShareNone}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.LoadShareStateContext(ctx); st.Revision != 3 {
		t.Fatalf("revision after SaveShare = %d, want 3", st.Revision)
	}

	// A refused change does not raise it.
	if _, err := s.CommitShareContext(ctx, ShareSelection{Mode: "bogus"}); err == nil {
		t.Fatal("an invalid mode was committed")
	}
	if _, err := s.CommitShareContext(ctx, ShareSelection{Mode: ShareSelect, Sessions: []string{""}}); err == nil {
		t.Fatal("an empty session name was committed")
	}
	if st, _ := s.LoadShareStateContext(ctx); st.Revision != 3 {
		t.Fatalf("a refused change raised the revision to %d", st.Revision)
	}

	// A reset removes the choice, and that is a change; the revision is not reset.
	res, err := s.ResetPairing(ctx)
	if err != nil || !res.Share {
		t.Fatalf("reset = %+v %v", res, err)
	}
	st, _ = s.LoadShareStateContext(ctx)
	if st.Revision != 4 || st.Configured || st.Mode != ShareLater {
		t.Fatalf("after a reset: %+v, want revision 4 and no choice", st)
	}
	if res, _ := s.ResetPairing(ctx); res.Share {
		t.Fatal("a second reset reported a choice")
	}
	if st, _ := s.LoadShareStateContext(ctx); st.Revision != 4 {
		t.Fatalf("a reset that removed nothing raised the revision to %d", st.Revision)
	}

	// It survives a reopen, and the next change continues from it.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, dir)
	if st, _ := s2.LoadShareStateContext(ctx); st.Revision != 4 {
		t.Fatalf("after a reopen: %d", st.Revision)
	}
	if got, _ := s2.CommitShareContext(ctx, ShareSelection{Mode: ShareAll}); got.Revision != 5 {
		t.Fatalf("next commit = %d, want 5", got.Revision)
	}
}
