package store

import (
	"context"
	"testing"
)

func TestOpRequestIsStoredOnceAndReadBack(t *testing.T) {
	st := openSettingsStore(t)
	ctx := context.Background()
	const now = int64(1_800_000_000_000)
	row := OpRequestRow{RequestID: "r1", Op: "grant_set", Actor: "u1", ArgsSHA256: "aa", ResultJSON: `{"status":200}`, CreatedAt: now}

	if _, ok, err := st.GetOpRequest(ctx, "r1", now); err != nil || ok {
		t.Fatalf("before the write: found=%v err=%v", ok, err)
	}
	if stored, err := st.PutOpRequest(ctx, row); err != nil || !stored {
		t.Fatalf("put: stored=%v err=%v", stored, err)
	}
	got, ok, err := st.GetOpRequest(ctx, "r1", now+1)
	if err != nil || !ok || got != row {
		t.Fatalf("get = %+v found=%v err=%v, want %+v", got, ok, err, row)
	}
	// The first answer of an id stays: a second write changes nothing.
	other := row
	other.Op, other.ResultJSON, other.CreatedAt = "grant_revoke", `{"status":409}`, now+5
	if stored, err := st.PutOpRequest(ctx, other); err != nil || stored {
		t.Fatalf("second put: stored=%v err=%v", stored, err)
	}
	if got, _, _ := st.GetOpRequest(ctx, "r1", now+6); got != row {
		t.Fatalf("after the second put = %+v, want %+v", got, row)
	}
}

func TestOpRequestRetentionIsTenMinutesSweptOnWrite(t *testing.T) {
	st := openSettingsStore(t)
	ctx := context.Background()
	const now = int64(1_800_000_000_000)
	keep := OpRequestRetention.Milliseconds()
	put := func(id string, at int64) {
		t.Helper()
		if _, err := st.PutOpRequest(ctx, OpRequestRow{RequestID: id, Op: "grant_set", Actor: "u1", ArgsSHA256: "aa", ResultJSON: `{}`, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM op_requests`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	put("old", now)

	// Just inside the retention the answer is still there; at its end the id
	// is free again, although no write has swept the row yet.
	if _, ok, _ := st.GetOpRequest(ctx, "old", now+keep-1); !ok {
		t.Fatal("the row is gone before the retention ended")
	}
	if _, ok, _ := st.GetOpRequest(ctx, "old", now+keep); ok {
		t.Fatal("the row is still answered after the retention")
	}
	if n := count(); n != 1 {
		t.Fatalf("rows before the sweep = %d, want 1", n)
	}

	// A write sweeps what is older than ten minutes and keeps the rest.
	put("mid", now+keep-1)
	if n := count(); n != 2 {
		t.Fatalf("rows after a write inside the retention = %d, want 2", n)
	}
	put("new", now+keep)
	if n := count(); n != 2 {
		t.Fatalf("rows after the sweep = %d, want 2 (mid, new)", n)
	}
	if _, ok, _ := st.GetOpRequest(ctx, "mid", now+keep); !ok {
		t.Fatal("a row inside the retention was swept")
	}
	// An expired id can be used again: the sweep of the same write frees it.
	put("old", now+keep+1)
	if got, ok, _ := st.GetOpRequest(ctx, "old", now+keep+2); !ok || got.CreatedAt != now+keep+1 {
		t.Fatalf("reused id = %+v found=%v", got, ok)
	}
}

func TestOpRequestNeedsItsMembers(t *testing.T) {
	st := openSettingsStore(t)
	for _, r := range []OpRequestRow{
		{Op: "grant_set", ArgsSHA256: "aa", ResultJSON: "{}"},
		{RequestID: "r", ArgsSHA256: "aa", ResultJSON: "{}"},
		{RequestID: "r", Op: "grant_set", ResultJSON: "{}"},
		{RequestID: "r", Op: "grant_set", ArgsSHA256: "aa"},
	} {
		if _, err := st.PutOpRequest(context.Background(), r); err == nil {
			t.Errorf("PutOpRequest(%+v) = nil, want an error", r)
		}
	}
}
