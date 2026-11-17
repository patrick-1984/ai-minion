package agent

import (
	"bytes"

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
	case stated.Mode == "own" && bytes.Equal(stated.KeyHash, seen.KeyHash[:]):
		return TLSPathKeysMatch, "keys match (the certificate has the key the Hub states it serves); " + TLSFacts(seen)
	case stated.Mode == "own":
		return TLSPathKeysDiffer, "keys differ: a TLS endpoint with another key is in between, such as a proxy; " + TLSFacts(seen)
	default:
		return TLSPathPlainToHub, "plain to Hub: the Hub received plain HTTP, so TLS ends before the Hub, at a proxy; " + TLSFacts(seen)
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
