package version

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// maxBundleSize bounds a cosign bundle; real ones are a few kilobytes.
const maxBundleSize = 1 << 20

// ErrVerification wraps every signature verification failure, so a caller can
// tell "the release is not trustworthy" from "the network failed".
var ErrVerification = errors.New("signature verification failed")

// BlobVerifier checks that a blob was signed by the release workflow of the
// release with the given tag. It is the port the updater depends on; the
// production implementation is [SigstoreVerifier].
type BlobVerifier interface {
	// VerifyBlob verifies bundleJSON (a cosign keyless bundle) over artifact
	// for the release tag, for example "v1.4.2".
	VerifyBlob(artifact, bundleJSON []byte, tag string) error
}

// SigstoreVerifier implements [BlobVerifier] with sigstore-go (AD-1: this is
// the only package that imports it). It requires:
//
//   - a certificate chain to the trusted Fulcio root;
//   - the certificate identity of the release workflow for the tag
//     (see [ReleaseIdentity]): SAN, OIDC issuer, source ref refs/tags/<tag>
//     and source repository;
//   - an entry in the Rekor transparency log with a verified inclusion proof
//     (a bare signed inclusion promise is not enough, see
//     [requireInclusionProofs]), which with the signed timestamp of the log
//     also supplies the time the short-lived certificate is checked at;
//   - a signature over exactly the artifact bytes.
type SigstoreVerifier struct {
	trusted root.TrustedMaterial
}

// NewSigstoreVerifier returns a verifier that trusts the given Sigstore
// material (the public-good trusted root in production).
func NewSigstoreVerifier(trusted root.TrustedMaterial) *SigstoreVerifier {
	return &SigstoreVerifier{trusted: trusted}
}

// VerifyBlob implements [BlobVerifier].
func (v *SigstoreVerifier) VerifyBlob(artifact, bundleJSON []byte, tag string) error {
	if err := v.verifyBlob(artifact, bundleJSON, tag); err != nil {
		return fmt.Errorf("%w: %w", ErrVerification, err)
	}
	return nil
}

func (v *SigstoreVerifier) verifyBlob(artifact, bundleJSON []byte, tag string) error {
	ver, err := ParseVersion(tag)
	if err != nil || ver.String() != tag {
		return fmt.Errorf("%q is not a release tag", tag)
	}
	if len(bundleJSON) == 0 || len(bundleJSON) > maxBundleSize {
		return errors.New("the signature bundle is missing or too large")
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("cannot read the signature bundle: %w", err)
	}

	if err := requireInclusionProofs(&b); err != nil {
		return err
	}

	id := ReleaseIdentity(tag)
	certID, err := verify.NewCertificateIdentity(
		verify.SubjectAlternativeNameMatcher{SubjectAlternativeName: id.SubjectAlternativeName},
		verify.IssuerMatcher{Issuer: id.Issuer},
		certificate.Extensions{
			SourceRepositoryRef: id.SourceRef,
			SourceRepositoryURI: id.SourceRepositoryURI,
		},
	)
	if err != nil {
		return err
	}

	sv, err := verify.NewVerifier(v.trusted,
		verify.WithTransparencyLog(1), // Rekor inclusion
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return err
	}
	res, err := sv.Verify(&b, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(artifact)),
		verify.WithCertificateIdentity(certID),
	))
	if err != nil {
		return err
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return errors.New("the bundle is not signed with a certificate")
	}
	// The policy above already enforces the identity. Checking the verified
	// certificate again keeps the rule visible and independent of how a
	// library version applies a policy.
	return CheckIdentity(*res.Signature.Certificate, id)
}

// requireInclusionProofs refuses a bundle with a transparency log entry that
// has no inclusion proof. sigstore-go accepts an entry that carries only a
// signed inclusion promise (a version 0.1 bundle), which is the log's promise
// to include the entry, not proof that it did. Requiring a proof on every
// entry means every entry that sigstore-go counts has had its proof checked
// against the trusted log (it verifies a proof whenever one is present).
func requireInclusionProofs(b *bundle.Bundle) error {
	entries, err := b.TlogEntries()
	if err != nil {
		return fmt.Errorf("cannot read the transparency log entries: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("the bundle has no transparency log entry")
	}
	for i, e := range entries {
		if !e.HasInclusionProof() {
			return fmt.Errorf("transparency log entry %d has no inclusion proof (an inclusion promise alone is not accepted)", i)
		}
	}
	return nil
}

// CheckIdentity compares the summary of a verified signing certificate with
// the wanted release identity. Every field must match exactly: a missing
// value never passes.
func CheckIdentity(got certificate.Summary, want Identity) error {
	switch {
	case got.SubjectAlternativeName != want.SubjectAlternativeName:
		return fmt.Errorf("certificate identity is %q, want the release workflow %q", got.SubjectAlternativeName, want.SubjectAlternativeName)
	case got.Issuer != want.Issuer:
		return fmt.Errorf("certificate issuer is %q, want %q", got.Issuer, want.Issuer)
	case got.SourceRepositoryRef != want.SourceRef:
		return fmt.Errorf("certificate source ref is %q, want %q", got.SourceRepositoryRef, want.SourceRef)
	case got.SourceRepositoryURI != want.SourceRepositoryURI:
		return fmt.Errorf("certificate source repository is %q, want %q", got.SourceRepositoryURI, want.SourceRepositoryURI)
	}
	return nil
}
