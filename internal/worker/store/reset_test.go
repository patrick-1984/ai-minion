package store

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
)

func TestResetPairingRemovesPinEnrollmentAndShareOnly(t *testing.T) {
	st := openStore(t, t.TempDir())
	ctx := context.Background()
	if err := st.SaveEnrollment(ctx, Enrollment{HubPub: make(ed25519.PublicKey, 32), WorkerID: "w1", Name: "pi", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveShareContext(ctx, ShareSelection{Mode: ShareSelect, Sessions: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.NextSessionNumberContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutResult(ctx, ResultRecord{RequestID: "r1", Op: "send_text", Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}

	got, err := st.ResetPairing(ctx)
	if err != nil || !got.Enrollment || !got.Share {
		t.Fatalf("ResetPairing = %+v, %v", got, err)
	}
	if _, err := st.LoadEnrollment(ctx); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("enrollment still there: %v", err)
	}
	sel, err := st.LoadShareContext(ctx)
	if err != nil || sel.Configured || sel.Mode != ShareLater || len(sel.Sessions) != 0 {
		t.Fatalf("share = %+v, %v", sel, err)
	}
	// The session counter continues: a name is never reused.
	if n, err := st.NextSessionNumberContext(ctx); err != nil || n != 4 {
		t.Fatalf("next session number = %d, %v; want 4", n, err)
	}
	if _, ok, err := st.GetResult(ctx, "r1"); err != nil || !ok {
		t.Fatalf("the result cache must stay: %v, %v", ok, err)
	}

	again, err := st.ResetPairing(ctx)
	if err != nil || again.Enrollment || again.Share {
		t.Fatalf("second ResetPairing = %+v, %v; want nothing removed", again, err)
	}
}
