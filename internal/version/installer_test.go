package version

import (
	"strings"
	"testing"
)

func TestInstallerDigestComesFromTheSignedManifestOfTheExactTag(t *testing.T) {
	sum := strings.Repeat("cd", 32)
	man, err := ParseManifest(manifestJSON("v1.2.3", "1.0", "1.1", `{"name":"install.sh","sha256":"`+sum+`"}`), "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	rel := Release{Tag: "v1.2.3", Assets: []Asset{{Name: "install.sh"}}}
	if got, err := InstallerDigest(rel, man); err != nil || got != sum {
		t.Fatalf("digest = %q, %v", got, err)
	}
	if _, err := InstallerDigest(Release{Tag: "v1.2.4", Assets: rel.Assets}, man); err == nil {
		t.Error("a manifest of another tag was accepted")
	}
	if _, err := InstallerDigest(Release{Tag: "v1.2.3"}, man); err == nil {
		t.Error("a release without the install.sh asset was accepted")
	}
	noScript, err := ParseManifest(manifestJSON("v1.2.3", "1.0", "1.1", ""), "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallerDigest(rel, noScript); err == nil {
		t.Error("a manifest without install.sh was accepted")
	}
}
