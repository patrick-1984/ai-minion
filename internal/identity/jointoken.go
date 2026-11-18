package identity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// Join tokens (v0.12.0): everything a new Worker needs to join one Hub, in one
// string that the "Add worker" page shows once. The blueprint is the join
// token of Docker Swarm (SWMTKN-1-<CA digest>-<secret>) and the kubeadm join
// command (--token plus --discovery-token-ca-cert-hash): the token carries the
// pin of the Hub together with a one-time secret, so the joining machine
// verifies the Hub before it sends the secret, and nobody has to copy a
// fingerprint and a key separately.
//
// The text form is
//
//	mnj1-<base64url without padding of the payload>
//
// "mnj" names the kind (MiNion Join), "1" the version of the payload. The
// payload is the compact JSON object {"hub":"<url>","key":"<invitation key>"}
// with exactly these two members in this order:
//
//   - hub is the Hub URL the Worker connects to (https, http, wss or ws, a
//     host, no user name or password, no query or fragment);
//   - key is the invitation key mnk_<fingerprint>.<secret> (AD-7). Its
//     fingerprint is the full 52-character fingerprint of the Hub's root key,
//     so the token pins the Hub with the full value, never with an
//     abbreviation (C1 of the v0.11 trust design), and the pin and the secret
//     can never disagree.
//
// Parsing is strict: one canonical encoding (the payload must re-encode to
// exactly the same bytes, base64url must be canonical), no unknown members,
// no trailing data. No cryptography is new here: the token only transports
// the existing invitation key and the existing fingerprint.

// JoinTokenPrefix starts every join token of version 1.
const JoinTokenPrefix = "mnj1-"

// maxJoinTokenLen bounds the text form; a real token is about 250 characters.
const maxJoinTokenLen = 4096

// JoinToken is a parsed join token. It holds a secret (the invitation key):
// String, GoString, LogValue and every fmt verb print a redacted form; only
// [JoinToken.Encode] returns the token itself.
type JoinToken struct {
	// HubURL is the address of the Hub, as given to [NewJoinToken].
	HubURL string
	// Invite is the one-time invitation key; its Fingerprint is the Hub's.
	Invite InviteKey
}

// joinPayload is the JSON payload; the field order is the canonical order.
type joinPayload struct {
	Hub string `json:"hub"`
	Key string `json:"key"`
}

// NewJoinToken builds the token for the Hub at hubURL and the invitation key
// invite. The URL must be a usable Hub address (see [ParseJoinToken]); the
// error never repeats the key.
func NewJoinToken(hubURL string, invite InviteKey) (JoinToken, error) {
	if err := ValidateJoinHubURL(hubURL); err != nil {
		return JoinToken{}, err
	}
	if _, err := ParseFingerprint(string(invite.Fingerprint)); err != nil || len(invite.Secret) != inviteSecretSize {
		return JoinToken{}, newErr(kindInvalidArgument, "join token: the invitation key is not valid")
	}
	return JoinToken{HubURL: hubURL, Invite: invite}, nil
}

// Fingerprint is the full fingerprint of the Hub the token pins.
func (t JoinToken) Fingerprint() Fingerprint { return t.Invite.Fingerprint }

// Encode returns the text form mnj1-.... It contains the secret: never log it.
func (t JoinToken) Encode() string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(joinPayload{Hub: t.HubURL, Key: t.Invite.String()})
	return JoinTokenPrefix + EncodeB64(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// redacted is the printable form: the public parts and no secret.
func (t JoinToken) redacted() string {
	return JoinTokenPrefix + "[redacted] (hub " + t.HubURL + ", fingerprint " + string(t.Invite.Fingerprint) + ")"
}

// String implements fmt.Stringer with the redacted form.
func (t JoinToken) String() string { return t.redacted() }

// GoString implements fmt.GoStringer with the redacted form.
func (t JoinToken) GoString() string { return t.redacted() }

// LogValue implements slog.LogValuer with the redacted form.
func (t JoinToken) LogValue() slog.Value { return slog.StringValue(t.redacted()) }

// Format implements fmt.Formatter: every verb prints the redacted form.
func (t JoinToken) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(t.redacted())) }

// LooksLikeJoinToken reports whether s starts like a join token of any
// version (mnj<digits>-), so that callers can tell a token from a URL or a
// path without parsing it.
func LooksLikeJoinToken(s string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "mnj")
	if !ok {
		return false
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	return digits > 0 && digits < len(rest) && rest[digits] == '-'
}

// ParseJoinToken parses the text form. Surrounding white space is ignored.
// Every failure is ErrInvalidArgument with a message that never repeats the
// input: a token of another version, a payload that is not the canonical
// JSON, an unknown member, a Hub URL that is not usable and an invitation key
// that is malformed.
func ParseJoinToken(s string) (JoinToken, error) {
	bad := func(why string) (JoinToken, error) {
		return JoinToken{}, newErr(kindInvalidArgument, "malformed join token: "+why)
	}
	s = strings.TrimSpace(s)
	if len(s) > maxJoinTokenLen {
		return bad("too long")
	}
	body, ok := strings.CutPrefix(s, JoinTokenPrefix)
	if !ok {
		if LooksLikeJoinToken(s) {
			return bad("this version of minion reads only mnj1- tokens; update minion")
		}
		return bad("it must start with " + JoinTokenPrefix)
	}
	raw, err := DecodeB64(body)
	if err != nil {
		return bad("the part after " + JoinTokenPrefix + " is not base64url")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p joinPayload
	if err := dec.Decode(&p); err != nil {
		return bad("the payload is not the expected JSON object")
	}
	if dec.More() {
		return bad("data after the payload")
	}
	invite, err := ParseInviteKey(p.Key)
	if err != nil {
		return bad("the invitation key in it is not valid")
	}
	t, err := NewJoinToken(p.Hub, invite)
	if err != nil {
		return bad("the Hub address in it is not usable")
	}
	// One canonical form: the same token re-encodes to the same text. This
	// rejects a reordered or re-spaced payload, duplicate members, escapes
	// and a fingerprint in capital letters.
	if t.Encode() != s {
		return bad("the payload is not in canonical form")
	}
	return t, nil
}

// ValidateJoinHubURL reports whether v can be the Hub address of a join
// token. It accepts the Hub addresses a Worker accepts as --server:
// scheme https, http, wss or ws, a host, no user name or password; and in a
// token additionally no query, no fragment, no white space and no control
// character. The error never repeats the URL.
func ValidateJoinHubURL(v string) error {
	bad := func(why string) error { return newErr(kindInvalidArgument, "join token: the Hub address "+why) }
	if v == "" || len(v) > 2048 {
		return bad("is empty or too long")
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return bad("contains white space or a control character")
		}
	}
	u, err := url.Parse(v)
	if err != nil {
		return bad("is not a valid URL")
	}
	switch u.Scheme {
	case "https", "http", "wss", "ws":
		if !strings.HasPrefix(v, u.Scheme+"://") {
			return bad("must start with https://, http://, wss:// or ws:// (lowercase)")
		}
	default:
		return bad("must start with https://, http://, wss:// or ws:// (lowercase)")
	}
	switch {
	case u.Hostname() == "":
		return bad("has no host")
	case u.User != nil:
		return bad("must not contain a user name or password")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(v, "?#"):
		return bad("must not have a query or a fragment")
	}
	return nil
}
