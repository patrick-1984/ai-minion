package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// HubTLSInfo is the optional hub_tls member of the welcome (v0.11.0): what the
// Hub states, inside the authenticated channel, about the TLS of this very
// connection. Mode "own" means the request arrived on the Hub's own TLS
// listener and KeyHash is the SHA-256 of the SubjectPublicKeyInfo of the
// certificate used for it; "off" means the Hub received plain HTTP. An older
// Hub omits the member.
type HubTLSInfo struct {
	Mode    string       `json:"mode"`
	KeyHash identity.B64 `json:"key_sha256,omitempty"`
}

// UnmarshalJSON decodes the member leniently: it is an optional diagnostic,
// so a malformed member never fails the welcome (and with it an authenticated
// pairing or connection). Whatever cannot be read is left empty, which
// [TLSPath] reports as unknown.
func (h *HubTLSInfo) UnmarshalJSON(data []byte) error {
	*h = HubTLSInfo{}
	var raw struct {
		Mode    json.RawMessage `json:"mode"`
		KeyHash json.RawMessage `json:"key_sha256"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	_ = json.Unmarshal(raw.Mode, &h.Mode)
	var key identity.B64
	if len(raw.KeyHash) > 0 && json.Unmarshal(raw.KeyHash, &key) == nil && len(key) == sha256.Size {
		h.KeyHash = key
	}
	return nil
}

// TLS path states (informational only, never a verdict). The comparison is a
// comparison of certificate keys in the style of RFC 5929: the key the Worker
// saw against the key the Hub states it served.
const (
	// TLSPathNone: the Worker dialled without TLS.
	TLSPathNone = "none"
	// TLSPathUnknown: the Hub does not report its own certificate (before
	// 0.11), or nothing is known yet.
	TLSPathUnknown = "unknown"
	// TLSPathKeysMatch: the certificate the Worker saw has the key the Hub
	// states it served.
	TLSPathKeysMatch = "keys_match"
	// TLSPathKeysDiffer: a TLS endpoint with another key is in between (a
	// proxy).
	TLSPathKeysDiffer = "keys_differ"
	// TLSPathPlainToHub: the Hub received plain HTTP, so TLS ends before the
	// Hub (a proxy).
	TLSPathPlainToHub = "plain_to_hub"
)

// TLSPath classifies one connection from what the Worker saw on its dial and
// what the Hub stated in the welcome (nil when it stated nothing). line is
// the text for the console, the log and `minion worker status`.
func TLSPath(seen transport.TLSSeen, stated *HubTLSInfo) (state, line string) {
	switch {
	case seen.None:
		return TLSPathNone, "none (plain connection). The Worker channel is still encrypted and authenticated."
	case !seen.Seen():
		return TLSPathUnknown, "not known"
	case stated == nil:
		return TLSPathUnknown, TLSFacts(seen) + "; this Hub does not report its own certificate"
	case stated.Mode == "own" && (len(stated.KeyHash) != sha256.Size || seen.KeyHash == [32]byte{}):
		// Both keys are needed for a comparison ("keys differ" only when
		// both exist): the Hub did not state a usable key (for example a
		// resumed TLS session), or the Worker has none.
		return TLSPathUnknown, TLSFacts(seen) + "; the Hub did not state the key it served on this connection"
	case stated.Mode == "own" && bytes.Equal(stated.KeyHash, seen.KeyHash[:]):
		return TLSPathKeysMatch, "keys match (the certificate has the key the Hub states it serves); " + TLSFacts(seen)
	case stated.Mode == "own":
		return TLSPathKeysDiffer, "keys differ: a TLS endpoint with another key is in between, such as a proxy; " + TLSFacts(seen)
	case stated.Mode == "off":
		return TLSPathPlainToHub, "plain to Hub: the Hub received plain HTTP, so TLS ends before the Hub, at a proxy; " + TLSFacts(seen)
	default:
		return TLSPathUnknown, TLSFacts(seen) + "; the Hub's statement about its TLS is not readable"
	}
}

// TLSFacts describes the certificate the Worker saw, as facts and never as a
// verdict: "certificate for <name>, issued by <issuer> | self-signed, trusted
// by this computer | not trusted by this computer (<reason>)".
func TLSFacts(seen transport.TLSSeen) string {
	if seen.None {
		return "none (plain connection)"
	}
	if !seen.Seen() {
		return "not known"
	}
	s := "certificate for " + orText(seen.Name, "(no name)")
	if seen.SelfSigned {
		s += ", self-signed"
	} else {
		s += ", issued by " + orText(seen.Issuer, "(no name)")
	}
	if seen.Trusted {
		return s + ", trusted by this computer"
	}
	if seen.Reason != "" {
		return s + ", not trusted by this computer (" + seen.Reason + ")"
	}
	return s + ", not trusted by this computer"
}

func orText(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
