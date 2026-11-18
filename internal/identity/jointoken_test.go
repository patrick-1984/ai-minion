package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func testJoinInvite(t *testing.T) InviteKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := FingerprintOf(pub)
	if err != nil {
		t.Fatal(err)
	}
	k, err := NewInviteKey(fp)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestJoinTokenRoundTrip(t *testing.T) {
	k := testJoinInvite(t)
	for _, hub := range []string{"https://hub.example.com", "https://hub.example:8443/minion", "http://10.0.0.5:8080", "wss://[::1]:9443"} {
		tok, err := NewJoinToken(hub, k)
		if err != nil {
			t.Fatalf("%s: %v", hub, err)
		}
		text := tok.Encode()
		if !strings.HasPrefix(text, JoinTokenPrefix) || strings.ContainsAny(text, "+/= \n") {
			t.Errorf("%s: token %q is not mnj1- and base64url", hub, text)
		}
		got, err := ParseJoinToken("  " + text + "\n")
		if err != nil {
			t.Fatalf("%s: parse: %v", hub, err)
		}
		if got.HubURL != hub || got.Invite.String() != k.String() || got.Fingerprint() != k.Fingerprint {
			t.Errorf("%s: round trip gave %v", hub, got)
		}
		if len(got.Fingerprint()) != FingerprintLen {
			t.Errorf("fingerprint %q is not the full value", got.Fingerprint())
		}
	}
}

func TestJoinTokenFixedVector(t *testing.T) {
	// The payload is the compact JSON {"hub":...,"key":...} in base64url
	// without padding: other implementations can rely on this.
	key := "mnk_" + strings.Repeat("a", 52) + "." + strings.Repeat("b", 51) + "q"
	payload := `{"hub":"https://hub.example.com","key":"` + key + `"}`
	text := JoinTokenPrefix + EncodeB64([]byte(payload))
	tok, err := ParseJoinToken(text)
	if err != nil {
		t.Fatal(err)
	}
	if tok.HubURL != "https://hub.example.com" || tok.Invite.String() != key || tok.Encode() != text {
		t.Errorf("vector: %v / %q", tok, tok.Encode())
	}
}

func TestJoinTokenNeverPrintsTheSecret(t *testing.T) {
	k := testJoinInvite(t)
	tok, err := NewJoinToken("https://hub.example.com", k)
	if err != nil {
		t.Fatal(err)
	}
	secret := encodeB32(k.Secret)
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("x", "token", tok)
	outs := []string{
		tok.String(), tok.GoString(), fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s %q %x", tok, tok, tok, tok, tok, tok),
		fmt.Sprintf("%v", []JoinToken{tok}), fmt.Sprintf("%+v", struct{ T JoinToken }{tok}), logged.String(),
	}
	for _, out := range outs {
		if strings.Contains(out, secret) || strings.Contains(out, tok.Encode()) {
			t.Errorf("printed the secret: %s", out)
		}
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("no redaction marker: %s", out)
		}
	}
	if !strings.Contains(tok.String(), string(k.Fingerprint)) || !strings.Contains(tok.String(), "https://hub.example.com") {
		t.Errorf("the public parts are missing: %s", tok)
	}
}

func TestParseJoinTokenIsStrict(t *testing.T) {
	k := testJoinInvite(t)
	good, err := NewJoinToken("https://hub.example.com", k)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(payload string) string { return JoinTokenPrefix + EncodeB64([]byte(payload)) }
	key := k.String()
	upperFp := "mnk_" + strings.ToUpper(string(k.Fingerprint)) + "." + encodeB32(k.Secret)
	cases := map[string]string{
		"empty":              "",
		"no prefix":          strings.TrimPrefix(good.Encode(), JoinTokenPrefix),
		"other version":      "mnj2-" + strings.TrimPrefix(good.Encode(), JoinTokenPrefix),
		"padding":            good.Encode() + "==",
		"std alphabet":       strings.NewReplacer("-", "+", "_", "/").Replace(good.Encode()[len(JoinTokenPrefix):]),
		"not json":           enc("hello"),
		"unknown member":     enc(`{"hub":"https://hub.example.com","key":"` + key + `","x":1}`),
		"reordered":          enc(`{"key":"` + key + `","hub":"https://hub.example.com"}`),
		"spaces":             enc(`{"hub": "https://hub.example.com", "key": "` + key + `"}`),
		"trailing data":      enc(`{"hub":"https://hub.example.com","key":"` + key + `"}{}`),
		"duplicate member":   enc(`{"hub":"https://a.example","hub":"https://hub.example.com","key":"` + key + `"}`),
		"escaped slash":      enc(`{"hub":"https:\/\/hub.example.com","key":"` + key + `"}`),
		"upper fingerprint":  enc(`{"hub":"https://hub.example.com","key":"` + upperFp + `"}`),
		"missing key":        enc(`{"hub":"https://hub.example.com"}`),
		"bad key":            enc(`{"hub":"https://hub.example.com","key":"mnk_abc.def"}`),
		"short fingerprint":  enc(`{"hub":"https://hub.example.com","key":"mnk_` + string(k.Fingerprint)[:20] + "." + encodeB32(k.Secret) + `"}`),
		"missing hub":        enc(`{"key":"` + key + `"}`),
		"ftp hub":            enc(`{"hub":"ftp://hub.example.com","key":"` + key + `"}`),
		"hub with user":      enc(`{"hub":"https://u:p@hub.example.com","key":"` + key + `"}`),
		"hub with query":     enc(`{"hub":"https://hub.example.com/?a=b","key":"` + key + `"}`),
		"hub with fragment":  enc(`{"hub":"https://hub.example.com/#x","key":"` + key + `"}`),
		"hub without host":   enc(`{"hub":"https://","key":"` + key + `"}`),
		"hub with space":     enc(`{"hub":"https://hub.example.com/a b","key":"` + key + `"}`),
		"upper-case scheme":  enc(`{"hub":"HTTPS://hub.example.com","key":"` + key + `"}`),
		"too long":           JoinTokenPrefix + strings.Repeat("A", maxJoinTokenLen),
		"token inside token": JoinTokenPrefix + good.Encode(),
	}
	for name, in := range cases {
		_, err := ParseJoinToken(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: error %v is not ErrInvalidArgument", name, err)
		}
		if strings.Contains(err.Error(), encodeB32(k.Secret)) || (in != "" && strings.Contains(err.Error(), in)) {
			t.Errorf("%s: the error repeats the input: %v", name, err)
		}
	}
	if _, err := ParseJoinToken("mnj7-abc"); err == nil || !strings.Contains(err.Error(), "update minion") {
		t.Errorf("a token of a later version: %v", err)
	}
}

func TestNewJoinTokenRejectsBadInput(t *testing.T) {
	k := testJoinInvite(t)
	if _, err := NewJoinToken("https://YOUR HUB", k); err == nil {
		t.Error("accepted a URL with a space")
	}
	if _, err := NewJoinToken("https://hub.example.com", InviteKey{}); err == nil {
		t.Error("accepted an empty invitation key")
	}
}

func TestLooksLikeJoinToken(t *testing.T) {
	for in, want := range map[string]bool{
		"mnj1-abc": true, " mnj12-x": true, "mnj-abc": false, "mnj1": false, "mnk_abc": false,
		"https://hub.example": false, "mnjx-1": false,
	} {
		if got := LooksLikeJoinToken(in); got != want {
			t.Errorf("LooksLikeJoinToken(%q) = %v", in, got)
		}
	}
}
