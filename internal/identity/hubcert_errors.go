package identity

// CertError reports why a hub certificate or its validity window was
// rejected. Reason is one of the Reason* constants (the names of the design,
// section 5). Two CertErrors match under errors.Is when the reasons are equal,
// so the ErrCert* sentinels can test any error returned by this file. The text
// never echoes certificate bytes.
type CertError struct {
	Reason string
}

// Certificate rejection reasons.
const (
	ReasonCertMalformed    = "cert_malformed"
	ReasonCertRootMismatch = "cert_root_mismatch"
	ReasonCertBadSignature = "cert_bad_signature"
	ReasonCertNotYetValid  = "cert_not_yet_valid"
	ReasonCertExpired      = "cert_expired"
)

// Error implements error.
func (e *CertError) Error() string { return "identity: " + e.Reason }

// Is reports whether target is a *CertError with the same reason.
func (e *CertError) Is(target error) bool {
	t, ok := target.(*CertError)
	return ok && t.Reason == e.Reason
}

// Sentinels for errors.Is.
var (
	ErrCertMalformed    = &CertError{Reason: ReasonCertMalformed}
	ErrCertRootMismatch = &CertError{Reason: ReasonCertRootMismatch}
	ErrCertBadSignature = &CertError{Reason: ReasonCertBadSignature}
	ErrCertNotYetValid  = &CertError{Reason: ReasonCertNotYetValid}
	ErrCertExpired      = &CertError{Reason: ReasonCertExpired}
)

// SealError reports a failure of the sealed root file. The text is fixed per
// reason and says nothing about the file content, the key or the passphrase.
type SealError struct {
	Reason string
}

// Sealed root rejection reasons.
const (
	ReasonPassphraseTooShort = "passphrase_too_short"
	ReasonWrongPassphrase    = "wrong_passphrase"
	ReasonSealedInvalid      = "sealed_root_invalid"
	ReasonSignerDestroyed    = "signer_destroyed"
	// ReasonSealedPayload is a sealed root whose header was accepted and the
	// key derived, but whose authentication or plaintext failed.
	ReasonSealedPayload = "sealed_root_payload"
)

// Error implements error.
func (e *SealError) Error() string { return "identity: " + e.Reason }

// Is reports whether target is a *SealError with the same reason.
// ErrSealedRootPayload also matches ErrSealedRootInvalid.
func (e *SealError) Is(target error) bool {
	t, ok := target.(*SealError)
	return ok && (t.Reason == e.Reason || (e.Reason == ReasonSealedPayload && t.Reason == ReasonSealedInvalid))
}

// Sentinels for errors.Is.
var (
	ErrPassphraseTooShort = &SealError{Reason: ReasonPassphraseTooShort}
	ErrWrongPassphrase    = &SealError{Reason: ReasonWrongPassphrase}
	ErrSealedRootInvalid  = &SealError{Reason: ReasonSealedInvalid}
	ErrSignerDestroyed    = &SealError{Reason: ReasonSignerDestroyed}
	// ErrSealedRootPayload: a defect found after the derivation (the work was
	// done). It matches ErrSealedRootInvalid too.
	ErrSealedRootPayload = &SealError{Reason: ReasonSealedPayload}
)
