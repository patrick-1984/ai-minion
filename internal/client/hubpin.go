package client

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// HubCertError is the [ErrHubCertificate] of a Hub call: the certificate the
// Hub presented was not accepted, and nothing was sent. It names the reason
// and the TLS key pin of the presented certificate, so that the user can
// compare that pin with the one the Hub host prints and then trust it.
type HubCertError struct {
	// Reason says in a few plain words why the verification failed (for
	// example "signed by an unknown authority"). Empty for a pin mismatch.
	Reason string
	// Pin is the key pin of the leaf certificate the Hub presented
	// (sha256//...), empty when the certificate is not known.
	Pin string
	// Expected lists the pins of --hub-tls-pin when the presented key matched
	// none of them; it is nil when the chain verification failed instead.
	Expected []string
}

// Error implements error. It never presents the pin as trustworthy by
// itself: it says to compare it with the Hub host first.
func (e *HubCertError) Error() string {
	if e.Expected != nil {
		return fmt.Sprintf("%v: the Hub presented a certificate with the key pin %s, but --hub-tls-pin (hub_tls_pin) expects %s. "+
			"Nothing was sent. This is a different key: check the \"TLS key pin\" line of `minion hub identity --words` on the Hub host "+
			"before you change the setting; if it does not show %s, something between you and the Hub answered instead",
			ErrHubCertificate, orUnknown(e.Pin), strings.Join(e.Expected, "; "), orUnknown(e.Pin))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%v by this client (%s).", ErrHubCertificate, e.Reason)
	if e.Pin != "" {
		fmt.Fprintf(&b, " Its key pin is %s.", e.Pin)
	}
	b.WriteString(" Compare it with the \"TLS key pin\" line of `minion hub identity --words` on the Hub host; " +
		"only when they match, trust it with --hub-tls-pin <pin> (MINION_HUB_TLS_PIN, hub_tls_pin), " +
		"or give the Hub's certificate with --hub-ca-file <file> (on the Hub host it is <data dir>/tls/cert.pem)")
	return b.String()
}

// Unwrap makes the error match [ErrHubCertificate].
func (e *HubCertError) Unwrap() error { return ErrHubCertificate }

func orUnknown(pin string) string {
	if pin == "" {
		return "(unknown)"
	}
	return pin
}

// checkPin accepts the connection when the key of the leaf certificate has
// one of pins. It is the whole verification of a pinned client.
func checkPin(cs tls.ConnectionState, pins []string) error {
	if len(cs.PeerCertificates) == 0 {
		return &HubCertError{Expected: pins}
	}
	got := identity.SPKIPin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
	if slices.Contains(pins, got) {
		return nil
	}
	return &HubCertError{Pin: got, Expected: pins}
}

// presentedPin returns the key pin of the leaf certificate a failed
// verification names, or "" when the error carries none.
func presentedPin(err error) string {
	var (
		verify      *tls.CertificateVerificationError
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		invalid     x509.CertificateInvalidError
		leaf        *x509.Certificate
	)
	switch {
	case errors.As(err, &verify) && len(verify.UnverifiedCertificates) > 0:
		leaf = verify.UnverifiedCertificates[0]
	case errors.As(err, &hostErr):
		leaf = hostErr.Certificate
	case errors.As(err, &invalid):
		leaf = invalid.Cert
	case errors.As(err, &unknownAuth):
		leaf = unknownAuth.Cert
	}
	if leaf == nil {
		return ""
	}
	return identity.SPKIPin(leaf.RawSubjectPublicKeyInfo)
}
