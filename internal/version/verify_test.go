package version

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func recordedVerifier(t *testing.T) *SigstoreVerifier {
	t.Helper()
	tr, err := root.NewTrustedRootFromJSON(readTestdata(t, "trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewSigstoreVerifier(tr)
}

// TestRecordedBundles verifies the bundles recorded in testdata/ (made by
// TestRecordFixtures): the valid one passes, and each way a release can be
// wrong fails with ErrVerification.
func TestRecordedBundles(t *testing.T) {
	v := recordedVerifier(t)
	sums := readTestdata(t, "checksums.txt")

	cases := []struct {
		name    string
		blob    []byte
		bundle  string
		tag     string
		wantErr string // substring; empty means success
	}{
		{name: "valid", blob: sums, bundle: "checksums.valid.sigstore.json", tag: recordedTag},
		{name: "wrong workflow identity", blob: sums, bundle: "checksums.wrong-identity.sigstore.json", tag: recordedTag, wantErr: "SAN"},
		{name: "wrong repository", blob: sums, bundle: "checksums.wrong-repo.sigstore.json", tag: recordedTag, wantErr: "SAN"},
		{name: "signed for another tag", blob: sums, bundle: "checksums.wrong-ref.sigstore.json", tag: recordedTag, wantErr: "SAN"},
		{name: "valid bundle presented for the next tag", blob: sums, bundle: "checksums.valid.sigstore.json", tag: "v1.2.4", wantErr: "SAN"},
		{name: "workflow run for a branch", blob: sums, bundle: "checksums.branch.sigstore.json", tag: recordedTag, wantErr: "SAN"},
		{name: "source ref extension differs", blob: sums, bundle: "checksums.wrong-ref-extension.sigstore.json", tag: recordedTag, wantErr: "SourceRepositoryRef"},
		{name: "wrong OIDC issuer", blob: sums, bundle: "checksums.wrong-issuer.sigstore.json", tag: recordedTag, wantErr: "issuer"},
		{name: "no Rekor entry", blob: sums, bundle: "checksums.no-rekor.sigstore.json", tag: recordedTag, wantErr: "log"},
		{name: "tampered checksums", blob: readTestdata(t, "checksums.tampered.txt"), bundle: "checksums.valid.sigstore.json", tag: recordedTag, wantErr: "invalid signature"},
		{name: "empty checksums", blob: nil, bundle: "checksums.valid.sigstore.json", tag: recordedTag, wantErr: "invalid signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v.VerifyBlob(tc.blob, readTestdata(t, tc.bundle), tc.tag)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyBlob: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("VerifyBlob accepted a bad release")
			}
			if !errors.Is(err, ErrVerification) {
				t.Errorf("error does not wrap ErrVerification: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyRejectsMalformedInput(t *testing.T) {
	v := recordedVerifier(t)
	sums := readTestdata(t, "checksums.txt")
	good := readTestdata(t, "checksums.valid.sigstore.json")
	for name, tc := range map[string]struct {
		bundle []byte
		tag    string
	}{
		"empty bundle":        {nil, recordedTag},
		"not json":            {[]byte("not a bundle"), recordedTag},
		"huge bundle":         {make([]byte, maxBundleSize+1), recordedTag},
		"tag without v":       {good, "1.2.3"},
		"channel as tag":      {good, "1.2"},
		"tag with whitespace": {good, "v1.2.3 "},
	} {
		t.Run(name, func(t *testing.T) {
			err := v.VerifyBlob(sums, tc.bundle, tc.tag)
			if !errors.Is(err, ErrVerification) {
				t.Fatalf("err = %v, want ErrVerification", err)
			}
		})
	}
}

// TestVerifyWithFreshSigstore signs at run time with a second private
// Sigstore: material from another root must not verify the first one's
// bundles (an unknown CA and log).
func TestVerifyWithFreshSigstore(t *testing.T) {
	a, b := newFakeSigstore(t), newFakeSigstore(t)
	blob := []byte("signed checksums")
	bundle := a.sign(t, blob, releaseSpec("v2.0.0", time.Now()))

	if err := NewSigstoreVerifier(a.material(t)).VerifyBlob(blob, bundle, "v2.0.0"); err != nil {
		t.Fatalf("own root: %v", err)
	}
	err := NewSigstoreVerifier(b.material(t)).VerifyBlob(blob, bundle, "v2.0.0")
	if !errors.Is(err, ErrVerification) {
		t.Fatalf("foreign root: err = %v, want ErrVerification", err)
	}
}

func TestVerifyRejectsEntryForOtherContent(t *testing.T) {
	f := newFakeSigstore(t)
	blob := []byte("checksums")
	spec := releaseSpec("v1.0.0", time.Now())
	spec.WrongBody = true
	err := NewSigstoreVerifier(f.material(t)).VerifyBlob(blob, f.sign(t, blob, spec), "v1.0.0")
	if !errors.Is(err, ErrVerification) {
		t.Fatalf("err = %v, want ErrVerification", err)
	}
}

func TestCheckIdentity(t *testing.T) {
	want := ReleaseIdentity("v1.4.2")
	good := certificate.Summary{
		SubjectAlternativeName: want.SubjectAlternativeName,
		Extensions: certificate.Extensions{
			Issuer:              want.Issuer,
			SourceRepositoryRef: want.SourceRef,
			SourceRepositoryURI: want.SourceRepositoryURI,
		},
	}
	if err := CheckIdentity(good, want); err != nil {
		t.Fatalf("good summary: %v", err)
	}
	mut := func(f func(*certificate.Summary)) certificate.Summary { s := good; f(&s); return s }
	for name, s := range map[string]certificate.Summary{
		"other SAN":     mut(func(s *certificate.Summary) { s.SubjectAlternativeName += "x" }),
		"empty SAN":     mut(func(s *certificate.Summary) { s.SubjectAlternativeName = "" }),
		"other issuer":  mut(func(s *certificate.Summary) { s.Issuer = "https://example.com" }),
		"empty issuer":  mut(func(s *certificate.Summary) { s.Issuer = "" }),
		"branch ref":    mut(func(s *certificate.Summary) { s.SourceRepositoryRef = "refs/heads/main" }),
		"missing ref":   mut(func(s *certificate.Summary) { s.SourceRepositoryRef = "" }),
		"other tag ref": mut(func(s *certificate.Summary) { s.SourceRepositoryRef = "refs/tags/v1.4.3" }),
		"other repo":    mut(func(s *certificate.Summary) { s.SourceRepositoryURI = "https://github.com/x/y" }),
		"missing repo":  mut(func(s *certificate.Summary) { s.SourceRepositoryURI = "" }),
	} {
		if err := CheckIdentity(s, want); err == nil {
			t.Errorf("%s: CheckIdentity accepted it", name)
		}
	}
}

func TestReleaseIdentityConstants(t *testing.T) {
	id := ReleaseIdentity("v0.1.0")
	if id.SubjectAlternativeName != "https://github.com/patrick-1984/ai-minion/.github/workflows/release.yml@refs/tags/v0.1.0" {
		t.Errorf("SAN = %q", id.SubjectAlternativeName)
	}
	if id.Issuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("issuer = %q", id.Issuer)
	}
	if id.SourceRef != "refs/tags/v0.1.0" || id.SourceRepositoryURI != "https://github.com/patrick-1984/ai-minion" {
		t.Errorf("ref/repo = %q %q", id.SourceRef, id.SourceRepositoryURI)
	}
	if BundleName(ChecksumsAsset) != "checksums.txt.sigstore.json" {
		t.Errorf("bundle name = %q", BundleName(ChecksumsAsset))
	}
}

// TestVerifyRequiresAnInclusionProof: a version 0.1 bundle may carry only the
// log's signed inclusion promise. That is a promise to include the entry, not
// a proof, so it must be refused, while the same bundle with its proof passes.
func TestVerifyRequiresAnInclusionProof(t *testing.T) {
	v := recordedVerifier(t)
	sums := readTestdata(t, "checksums.txt")
	var doc map[string]any
	if err := json.Unmarshal(readTestdata(t, "checksums.valid.sigstore.json"), &doc); err != nil {
		t.Fatal(err)
	}
	doc["mediaType"] = "application/vnd.dev.sigstore.bundle+json;version=0.1"
	entries := doc["verificationMaterial"].(map[string]any)["tlogEntries"].([]any)
	if len(entries) == 0 || entries[0].(map[string]any)["inclusionPromise"] == nil {
		t.Fatal("the recorded bundle has no inclusion promise to rely on")
	}
	encode := func() []byte {
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := v.VerifyBlob(sums, encode(), recordedTag); err != nil {
		t.Fatalf("promise and proof: %v", err)
	}
	for _, e := range entries {
		delete(e.(map[string]any), "inclusionProof")
	}
	err := v.VerifyBlob(sums, encode(), recordedTag)
	if !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), "inclusion proof") {
		t.Fatalf("promise-only bundle: err = %v, want an ErrVerification about the inclusion proof", err)
	}
}
