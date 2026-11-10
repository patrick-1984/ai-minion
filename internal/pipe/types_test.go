package pipe

import (
	"bytes"
	"testing"
	"time"
)

func TestNewTurnIDKnownVector(t *testing.T) {
	at := time.UnixMilli(0x018f0000abcd)
	id, err := NewTurnIDFrom(at, bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}))
	if err != nil {
		t.Fatal(err)
	}
	want := "018f0000-abcd-7fff-bfff-ffffffffffff"
	if id != want {
		t.Fatalf("id = %s, want %s", id, want)
	}
	if !IsTurnID(id) {
		t.Fatal("generated id is not a UUIDv7")
	}
}

func TestNewTurnIDSortsByTimeAndIsUnique(t *testing.T) {
	base := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 200; i++ {
		id, err := NewTurnID(base.Add(time.Duration(i) * time.Millisecond))
		if err != nil || !IsTurnID(id) {
			t.Fatalf("id %q err %v", id, err)
		}
		if seen[id] || id <= prev {
			t.Fatalf("not unique/ordered: %s after %s", id, prev)
		}
		seen[id], prev = true, id
	}
}

func TestNewTurnIDErrors(t *testing.T) {
	if _, err := NewTurnIDFrom(time.UnixMilli(-1), bytes.NewReader(make([]byte, 10))); err == nil {
		t.Fatal("negative time")
	}
	if _, err := NewTurnIDFrom(time.Now(), bytes.NewReader([]byte{1, 2})); err == nil {
		t.Fatal("short random source")
	}
}

func TestIsTurnID(t *testing.T) {
	for id, want := range map[string]bool{
		"018f0000-abcd-7fff-bfff-ffffffffffff": true,
		"018f0000-abcd-7000-8000-000000000000": true,
		"018f0000-abcd-7000-9000-000000000000": true,
		"018f0000-abcd-7000-a000-000000000000": true,
		"018f0000-abcd-4fff-bfff-ffffffffffff": false, // version 4
		"018f0000-abcd-7fff-cfff-ffffffffffff": false, // variant 11
		"018f0000-abcd-7fff-7fff-ffffffffffff": false, // variant 01
		"018F0000-ABCD-7FFF-BFFF-FFFFFFFFFFFF": false, // not canonical lower case
		"018f0000abcd7fffbfffffffffffffff":     false,
		"018f0000-abcd-7fff-bfff-fffffffffffg": false,
		"":                                     false,
	} {
		if got := IsTurnID(id); got != want {
			t.Errorf("IsTurnID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestPeer(t *testing.T) {
	p := Peer{WorkerID: "w", SessionName: "s"}
	if !p.Valid() || p.String() != "w/s" || (Peer{}).Valid() {
		t.Fatal("peer")
	}
}
