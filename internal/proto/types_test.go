package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestFrameTypeConstants(t *testing.T) {
	want := map[string]string{
		TypeHello: "hello", TypeSig: "sig", TypePakeMsg: "pake.msg", TypePakeConfirm: "pake.confirm",
		TypeInviteSecret: "invite.secret", TypeWelcome: "welcome", TypeRevoke: "revoke",
		TypeNotEnrolled: "not_enrolled", TypeOpRequest: "op.request", TypeOpProgress: "op.progress",
		TypeOpResult: "op.result", TypeError: "error", TypeEventsSnap: "events.snapshot",
		TypeEvent: "event", TypeTurnAck: "turn.ack", TypePing: "ping", TypePong: "pong",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("constant = %q, want %q", got, w)
		}
	}
	if len(want) != 17 {
		t.Fatalf("expected 17 distinct frame types, have %d", len(want))
	}
}

func TestStreamKindPrefixesAreTwoBytes(t *testing.T) {
	cases := []struct {
		kind   StreamKind
		prefix string
		name   string
	}{
		{StreamOp, "op", "op"},
		{StreamCall, "ca", "call"},
		{StreamEvents, "ev", "ev"},
	}
	for _, c := range cases {
		if !bytes.Equal(c.kind.Prefix(), []byte(c.prefix)) || len(c.kind.Prefix()) != StreamKindPrefixLen {
			t.Errorf("%s prefix = %q", c.name, c.kind.Prefix())
		}
		if c.kind.Name() != c.name {
			t.Errorf("name = %q, want %q", c.kind.Name(), c.name)
		}
		k, ok := ParseStreamKind(append(c.kind.Prefix(), "rest"...))
		if !ok || k != c.kind {
			t.Errorf("parse %q = %q, %v", c.prefix, k, ok)
		}
	}
	for _, bad := range []string{"", "o", "xx", "OP", "e"} {
		if k, ok := ParseStreamKind([]byte(bad)); ok {
			t.Errorf("parse %q accepted as %q", bad, k)
		}
	}
	// Every prefix is exactly two bytes and the four-byte word "call" is never
	// a prefix. ParseStreamKind reads only the first two bytes, so "call"
	// parses as StreamCall and the remaining "ll" belongs to the stream payload.
	for _, c := range cases {
		if len(c.kind.Prefix()) != StreamKindPrefixLen || string(c.kind.Prefix()) == "call" {
			t.Errorf("%s: prefix %q must be %d bytes and not \"call\"", c.name, c.kind.Prefix(), StreamKindPrefixLen)
		}
	}
	if k, ok := ParseStreamKind([]byte("call")); !ok || k != StreamCall {
		t.Errorf("ParseStreamKind(call) = %q, %v, want %q, true", k, ok, StreamCall)
	}
	if k, ok := ParseStreamKind([]byte("opxx")); !ok || k != StreamOp {
		t.Errorf("only the first two bytes may matter: got %q, %v", k, ok)
	}
}

func TestErrorFrameRoundTrip(t *testing.T) {
	orig := NewError(CodeBusy, "session busy")
	orig.Details = json.RawMessage(`{"pane":"%3"}`)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, StreamFrameLimit, orig.Frame()); err != nil {
		t.Fatalf("write error frame: %v", err)
	}
	if got := buf.String()[4:]; got != `{"type":"error","code":"busy","message":"session busy","details":{"pane":"%3"}}` {
		t.Fatalf("wire body = %s", got)
	}
	body, err := ReadFrame(&buf, StreamFrameLimit)
	if err != nil {
		t.Fatal(err)
	}
	if ty, err := FrameType(body); err != nil || ty != TypeError {
		t.Fatalf("FrameType = %q, %v", ty, err)
	}
	var f ErrorFrame
	if err := DecodeFrame(body, &f); err != nil {
		t.Fatal(err)
	}
	back := f.Err()
	if !errors.Is(back, orig) || back.Code != CodeBusy || back.Message != "session busy" || string(back.Details) != `{"pane":"%3"}` {
		t.Fatalf("Err() = %+v", back)
	}
	// A zero-value ErrorFrame has no type; Frame() always sets it.
	if (&Error{Code: CodeInternal}).Frame().Type != TypeError {
		t.Fatal("Frame() must set Type to error")
	}
	if err := WriteFrame(io.Discard, StreamFrameLimit, orig); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a bare Error is not a frame, err = %v", err)
	}
}

func TestErrorMatchingAndEnvelope(t *testing.T) {
	e := NewError(CodeNotFound, "no such thing")
	wrapped := fmt.Errorf("outer: %w", e)
	if !errors.Is(wrapped, &Error{Code: CodeNotFound}) || errors.Is(wrapped, ErrFrameTooLarge) {
		t.Fatal("errors.Is must match on code only")
	}
	if CodeOf(wrapped) != CodeNotFound || CodeOf(errors.New("x")) != CodeInternal {
		t.Fatal("CodeOf")
	}
	if e.Error() != "not_found: no such thing" || (&Error{Code: CodeBusy}).Error() != "busy" {
		t.Fatalf("Error() = %q", e.Error())
	}
	b, _ := json.Marshal(e)
	if string(b) != `{"code":"not_found","message":"no such thing"}` {
		t.Fatalf("json = %s", b)
	}
}

// The turn channel frame names are on the wire (AM-7): the Hub and the Worker
// both use these constants.
func TestTurnChannelFrameNames(t *testing.T) {
	if TypeTurnsOpen != "turns.open" || TypeTurn != "turn" || TypeTurnAck != "turn.ack" {
		t.Fatalf("turn frame names = %q %q %q", TypeTurnsOpen, TypeTurn, TypeTurnAck)
	}
}
