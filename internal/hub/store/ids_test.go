package store

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewIDIsUUIDv7(t *testing.T) {
	id := NewID()
	if err := ParseID(id); err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[14] != '7' {
		t.Fatalf("id = %q", id)
	}
}

func TestNewIDIsUniqueAndTimeOrdered(t *testing.T) {
	const n = 2000
	ids := make([]string, n)
	seen := map[string]bool{}
	for i := range ids {
		ids[i] = NewID()
		if seen[ids[i]] {
			t.Fatalf("duplicate id %s", ids[i])
		}
		seen[ids[i]] = true
		if i%500 == 0 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("UUIDv7 text must sort in creation order")
	}
}

func TestParseIDRejects(t *testing.T) {
	bad := []string{
		"",
		"not-a-uuid",
		"550e8400-e29b-41d4-a716-446655440000",  // v4
		strings.ToUpper(NewID()),                // not canonical lower case
		"01890a5d-ac96-774b-bcce-b302099a8057 ", // trailing space
		"01890a5d-ac96-374b-bcce-b302099a8057",  // v3
		"01890a5d-ac96-774b-0cce-b302099a8057",  // bad variant
	}
	for _, s := range bad {
		if err := ParseID(s); err == nil {
			t.Errorf("ParseID(%q) = nil, want error", s)
		}
	}
	if err := ParseID("01890a5d-ac96-774b-bcce-b302099a8057"); err != nil {
		t.Errorf("valid v7 rejected: %v", err)
	}
}

func TestUnixMSRoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 9, 12, 30, 45, 123_456_789, time.FixedZone("x", 3600))
	ms := UnixMS(ts)
	if ms != ts.UnixMilli() {
		t.Fatalf("ms = %d", ms)
	}
	back := FromUnixMS(ms)
	if back.Location() != time.UTC {
		t.Fatalf("location = %v", back.Location())
	}
	if !back.Equal(ts.Truncate(time.Millisecond)) {
		t.Fatalf("back = %v, want %v", back, ts.Truncate(time.Millisecond))
	}
	if d := time.Since(FromUnixMS(NowMS())); d < 0 || d > time.Second {
		t.Fatalf("NowMS off by %v", d)
	}
}
