package version

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	prototrustroot "github.com/sigstore/protobuf-specs/gen/pb-go/trustroot/v1"
	"github.com/sigstore/rekor/pkg/generated/models"
	rekorutil "github.com/sigstore/rekor/pkg/util"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A tiny private Sigstore: a Fulcio-like CA that issues certificates with the
// GitHub Actions extensions, and a Rekor-like log that returns a signed entry
// timestamp and a one-leaf inclusion proof. It produces bundles in the same
// shape cosign writes (bundle v0.3, message signature, certificate, tlog entry
// with promise and proof). Tests use it at run time to sign what they build,
// and "go test -run TestRecordFixtures -update" writes testdata/ from it.

var (
	oidIssuerV1  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	oidIssuerV2  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	oidSourceURI = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 12}
	oidSourceRef = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 14}
)

// fixtureEpoch is the fixed signing time of the recorded fixtures, so that
// they stay valid however late they are verified (the certificate is checked
// at the log's integrated time).
var fixtureEpoch = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

type fakeSigstore struct {
	rootCert  *x509.Certificate
	interCert *x509.Certificate
	interKey  *ecdsa.PrivateKey
	rekorKey  *ecdsa.PrivateKey
	logIDRaw  []byte
	logIndex  int64 // always 0: one-leaf trees
}

// chk returns v and panics on a setup error, which fails the test run.
func chk[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func newFakeSigstore(t testing.TB) *fakeSigstore {
	t.Helper()
	f := &fakeSigstore{}
	rootKey := chk(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "minion-test-root", Organization: []string{"minion test"}},
		NotBefore:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2046, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER := chk(x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey))
	f.rootCert = chk(x509.ParseCertificate(rootDER))

	f.interKey = chk(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	interTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "minion-test-intermediate", Organization: []string{"minion test"}},
		NotBefore:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2046, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	interDER := chk(x509.CreateCertificate(rand.Reader, interTmpl, f.rootCert, &f.interKey.PublicKey, rootKey))
	f.interCert = chk(x509.ParseCertificate(interDER))

	f.rekorKey = chk(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	pub := chk(x509.MarshalPKIXPublicKey(f.rekorKey.Public()))
	id := sha256.Sum256(pub)
	f.logIDRaw = id[:]
	return f
}

// trustedRootJSON is the trusted_root.json of this private Sigstore.
func (f *fakeSigstore) trustedRootJSON(t testing.TB) []byte {
	t.Helper()
	pub := chk(x509.MarshalPKIXPublicKey(f.rekorKey.Public()))
	tr := &prototrustroot.TrustedRoot{
		MediaType: root.TrustedRootMediaType01,
		Tlogs: []*prototrustroot.TransparencyLogInstance{{
			BaseUrl:       "https://rekor.localhost",
			HashAlgorithm: protocommon.HashAlgorithm_SHA2_256,
			PublicKey: &protocommon.PublicKey{
				RawBytes:   pub,
				KeyDetails: protocommon.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256,
				ValidFor:   &protocommon.TimeRange{Start: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))},
			},
			LogId: &protocommon.LogId{KeyId: f.logIDRaw},
		}},
		CertificateAuthorities: []*prototrustroot.CertificateAuthority{{
			Subject: &protocommon.DistinguishedName{Organization: "minion test", CommonName: "minion-test-root"},
			Uri:     "https://fulcio.localhost",
			CertChain: &protocommon.X509CertificateChain{Certificates: []*protocommon.X509Certificate{
				{RawBytes: f.interCert.Raw}, {RawBytes: f.rootCert.Raw},
			}},
			ValidFor: &protocommon.TimeRange{Start: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))},
		}},
	}
	return chk(protojson.Marshal(tr))
}

func (f *fakeSigstore) material(t testing.TB) root.TrustedMaterial {
	t.Helper()
	return chk(root.NewTrustedRootFromJSON(f.trustedRootJSON(t)))
}

// signSpec describes the identity a bundle is made for.
type signSpec struct {
	SAN       string
	Issuer    string
	Ref       string
	RepoURI   string
	At        time.Time
	OmitTlog  bool
	WrongBody bool // log an entry for different content
}

// releaseSpec is the identity of a correct release signature for tag.
func releaseSpec(tag string, at time.Time) signSpec {
	id := ReleaseIdentity(tag)
	return signSpec{SAN: id.SubjectAlternativeName, Issuer: id.Issuer, Ref: id.SourceRef, RepoURI: id.SourceRepositoryURI, At: at}
}

func derString(t testing.TB, s string) []byte {
	return chk(asn1.MarshalWithParams(s, "utf8"))
}

// sign returns a bundle (JSON) that signs blob as spec says.
func (f *fakeSigstore) sign(t testing.TB, blob []byte, spec signSpec) []byte {
	t.Helper()
	leafKey := chk(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	san := chk(url.Parse(spec.SAN))
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		URIs:         []*url.URL{san},
		NotBefore:    spec.At.Add(-time.Minute),
		NotAfter:     spec.At.Add(9 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		ExtraExtensions: []pkix.Extension{
			{Id: oidIssuerV1, Value: []byte(spec.Issuer)},
			{Id: oidIssuerV2, Value: derString(t, spec.Issuer)},
			{Id: oidSourceURI, Value: derString(t, spec.RepoURI)},
			{Id: oidSourceRef, Value: derString(t, spec.Ref)},
		},
	}
	leafDER := chk(x509.CreateCertificate(rand.Reader, tmpl, f.interCert, &leafKey.PublicKey, f.interKey))
	leaf := chk(x509.ParseCertificate(leafDER))

	digest := sha256.Sum256(blob)
	sig := chk(ecdsa.SignASN1(rand.Reader, leafKey, digest[:]))

	b := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: leaf.Raw}},
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: digest[:]},
			Signature:     sig,
		}},
	}
	if !spec.OmitTlog {
		entryDigest := digest
		if spec.WrongBody {
			entryDigest = sha256.Sum256(append([]byte("other:"), blob...))
		}
		b.VerificationMaterial.TlogEntries = []*protorekor.TransparencyLogEntry{f.logEntry(t, leaf, entryDigest[:], sig, spec.At)}
	}
	return chk(protojson.Marshal(b))
}

// logEntry builds a Rekor v1 hashedrekord entry with a signed entry
// timestamp and an inclusion proof for a one-leaf tree.
func (f *fakeSigstore) logEntry(t testing.TB, leaf *x509.Certificate, digest, sig []byte, at time.Time) *protorekor.TransparencyLogEntry {
	t.Helper()
	pem := chk(cryptoutils.MarshalCertificateToPEM(leaf))
	// Canonical form: keys sorted, no whitespace.
	body := fmt.Sprintf(`{"apiVersion":"0.0.1","kind":"hashedrekord","spec":{"data":{"hash":{"algorithm":"sha256","value":"%s"}},"signature":{"content":"%s","publicKey":{"content":"%s"}}}}`,
		hex.EncodeToString(digest), base64.StdEncoding.EncodeToString(sig), base64.StdEncoding.EncodeToString(pem))
	bodyRaw := []byte(body)

	// Every entry is the only leaf of its own one-leaf tree, at index 0.
	integrated := at.Unix()
	payload := tlog.RekorPayload{
		Body:           base64.StdEncoding.EncodeToString(bodyRaw),
		IntegratedTime: integrated,
		LogIndex:       f.logIndex,
		LogID:          hex.EncodeToString(f.logIDRaw),
	}
	payJSON := chk(json.Marshal(payload))
	canon := chk(jsoncanonicalizer.Transform(payJSON))
	h := sha256.Sum256(canon)
	set := chk(ecdsa.SignASN1(rand.Reader, f.rekorKey, h[:]))

	// A one-leaf tree: the root hash is the leaf hash of the body.
	leafHash := sha256.Sum256(append([]byte{0}, bodyRaw...))
	signer := chk(signature.LoadECDSASignerVerifier(f.rekorKey, crypto.SHA256))
	checkpoint := chk(rekorutil.CreateAndSignCheckpoint(context.Background(), "rekor.localhost", 123, 1, leafHash[:], signer))
	_ = models.InclusionProof{}

	return &protorekor.TransparencyLogEntry{
		LogIndex:          f.logIndex,
		LogId:             &protocommon.LogId{KeyId: f.logIDRaw},
		KindVersion:       &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"},
		IntegratedTime:    integrated,
		InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: set},
		CanonicalizedBody: bodyRaw,
		InclusionProof: &protorekor.InclusionProof{
			LogIndex:   f.logIndex,
			RootHash:   leafHash[:],
			TreeSize:   1,
			Hashes:     nil,
			Checkpoint: &protorekor.Checkpoint{Envelope: string(checkpoint)},
		},
	}
}

var _ = bytes.Equal
