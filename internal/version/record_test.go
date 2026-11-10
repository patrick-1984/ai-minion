package version

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -update rewrites testdata/ from a fresh private Sigstore:
//
//	go test ./internal/version -run TestRecordFixtures -update
//
// The recorded files are signed at a fixed time (fixtureEpoch) inside the
// validity of the recorded trusted root, so they verify forever. They are
// checked by TestRecordedBundles.
var update = flag.Bool("update", false, "rewrite the recorded fixtures in testdata/")

const recordedTag = "v1.2.3"

var recordedChecksums = []byte(strings.Join([]string{
	"1111111111111111111111111111111111111111111111111111111111111111  minion_1.2.3_linux_amd64.tar.gz",
	"2222222222222222222222222222222222222222222222222222222222222222  minion_1.2.3_darwin_arm64.tar.gz",
	"",
}, "\n"))

func TestRecordFixtures(t *testing.T) {
	if !*update {
		t.Skip("run with -update to rewrite testdata/")
	}
	f := newFakeSigstore(t)
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join("testdata", name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	good := releaseSpec(recordedTag, fixtureEpoch)

	write("trusted_root.json", f.trustedRootJSON(t))
	write("checksums.txt", recordedChecksums)
	write("checksums.valid.sigstore.json", f.sign(t, recordedChecksums, good))

	otherWorkflow := good
	otherWorkflow.SAN = RepositoryURI + "/.github/workflows/ci.yml@" + SourceRef(recordedTag)
	write("checksums.wrong-identity.sigstore.json", f.sign(t, recordedChecksums, otherWorkflow))

	otherRepo := good
	otherRepo.SAN = "https://github.com/someone-else/ai-minion/" + WorkflowPath + "@" + SourceRef(recordedTag)
	otherRepo.RepoURI = "https://github.com/someone-else/ai-minion"
	write("checksums.wrong-repo.sigstore.json", f.sign(t, recordedChecksums, otherRepo))

	// Signed for another tag: valid for v1.2.4, wrong for v1.2.3.
	write("checksums.wrong-ref.sigstore.json", f.sign(t, recordedChecksums, releaseSpec("v1.2.4", fixtureEpoch)))

	// Right workflow, but run for a branch, not for the tag.
	branch := good
	branch.SAN = RepositoryURI + "/" + WorkflowPath + "@refs/heads/main"
	branch.Ref = "refs/heads/main"
	write("checksums.branch.sigstore.json", f.sign(t, recordedChecksums, branch))

	// Right SAN, but the source-ref extension says another ref.
	extRef := good
	extRef.Ref = SourceRef("v9.9.9")
	write("checksums.wrong-ref-extension.sigstore.json", f.sign(t, recordedChecksums, extRef))

	badIssuer := good
	badIssuer.Issuer = "https://accounts.example.com"
	write("checksums.wrong-issuer.sigstore.json", f.sign(t, recordedChecksums, badIssuer))

	noLog := good
	noLog.OmitTlog = true
	write("checksums.no-rekor.sigstore.json", f.sign(t, recordedChecksums, noLog))

	tampered := []byte(strings.Replace(string(recordedChecksums), "1111", "6666", 1))
	write("checksums.tampered.txt", tampered)
}
