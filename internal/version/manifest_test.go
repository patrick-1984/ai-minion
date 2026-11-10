package version

import (
	"strings"
	"testing"
)

const sha1x = "1111111111111111111111111111111111111111111111111111111111111111"
const sha2x = "2222222222222222222222222222222222222222222222222222222222222222"

func manifestJSON(version, min, max string, artifacts string) []byte {
	return []byte(`{"schema":1,"version":"` + version + `","protocol_range":{"min":"` + min + `","max":"` + max + `"},"artifacts":[` + artifacts + `]}`)
}

func TestParseManifest(t *testing.T) {
	data := manifestJSON("v1.2.3", "1.0", "1.1",
		`{"name":"minion_1.2.3_linux_amd64.tar.gz","sha256":"`+sha1x+`"},{"name":"bootstrap.sh","sha256":"`+sha2x+`"}`)
	m, err := ParseManifest(data, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Range()
	if err != nil || r.String() != "1.0..1.1" {
		t.Fatalf("Range = %v, %v", r, err)
	}
	if d, ok := m.Digest("bootstrap.sh"); !ok || d != sha2x {
		t.Errorf("Digest = %q, %v", d, ok)
	}
	if _, ok := m.Digest("missing"); ok {
		t.Error("Digest found a missing artifact")
	}
}

func TestParseManifestRejects(t *testing.T) {
	art := `{"name":"a.tar.gz","sha256":"` + sha1x + `"}`
	cases := map[string][]byte{
		"not json":              []byte("{"),
		"other schema":          []byte(`{"schema":2,"version":"v1.2.3","protocol_range":{"min":"1.0","max":"1.0"},"artifacts":[]}`),
		"missing schema":        []byte(`{"version":"v1.2.3","protocol_range":{"min":"1.0","max":"1.0"}}`),
		"other version":         manifestJSON("v1.2.4", "1.0", "1.0", art),
		"bad range":             manifestJSON("v1.2.3", "1.1", "1.0", art),
		"range across majors":   manifestJSON("v1.2.3", "1.0", "2.0", art),
		"unparsable range":      manifestJSON("v1.2.3", "one", "1.0", art),
		"empty range":           manifestJSON("v1.2.3", "", "", art),
		"path in artifact name": manifestJSON("v1.2.3", "1.0", "1.0", `{"name":"../a","sha256":"`+sha1x+`"}`),
		"short digest":          manifestJSON("v1.2.3", "1.0", "1.0", `{"name":"a","sha256":"abcd"}`),
		"upper-case digest":     manifestJSON("v1.2.3", "1.0", "1.0", `{"name":"a","sha256":"`+strings.ToUpper(strings.Repeat("ab", 32))+`"}`),
		"non-hex digest":        manifestJSON("v1.2.3", "1.0", "1.0", `{"name":"a","sha256":"`+strings.Repeat("zz", 32)+`"}`),
	}
	for name, data := range cases {
		if _, err := ParseManifest(data, "v1.2.3"); err == nil {
			t.Errorf("%s: ParseManifest succeeded", name)
		}
	}
}

func TestParseManifestIgnoresUnknownFields(t *testing.T) {
	data := []byte(`{"schema":1,"future":true,"version":"v1.2.3","protocol_range":{"min":"1.0","max":"1.0","note":"x"},"artifacts":[]}`)
	if _, err := ParseManifest(data, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
}

func TestParseChecksums(t *testing.T) {
	ok := sha1x + "  minion_1.2.3_linux_amd64.tar.gz\n" + sha2x + " *minion_1.2.3_windows_amd64_preview.zip\r\n\n"
	c, err := ParseChecksums([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if c["minion_1.2.3_linux_amd64.tar.gz"] != sha1x || c["minion_1.2.3_windows_amd64_preview.zip"] != sha2x || len(c) != 2 {
		t.Errorf("got %v", c)
	}
	bad := map[string]string{
		"empty":            "",
		"one space":        sha1x + " name\n",
		"short digest":     "abcd  name\n",
		"no name":          sha1x + "  \n",
		"path":             sha1x + "  dir/name\n",
		"upper digest":     strings.ToUpper(strings.Repeat("ab", 32)) + "  name\n",
		"conflicting dups": sha1x + "  name\n" + sha2x + "  name\n",
		"garbage":          "hello world\n",
	}
	for name, in := range bad {
		if _, err := ParseChecksums([]byte(in)); err == nil {
			t.Errorf("%s: ParseChecksums succeeded", name)
		}
	}
	if _, err := ParseChecksums([]byte(sha1x + "  name\n" + sha1x + "  name\n")); err != nil {
		t.Errorf("identical duplicate: %v", err)
	}
}

// The names are the ones .goreleaser.yaml produces (Story 9.1).
func TestArchiveName(t *testing.T) {
	cases := []struct{ tag, os, arch, arm, want string }{
		{"v0.1.0", "linux", "amd64", "", "minion_0.1.0_linux_amd64.tar.gz"},
		{"v0.1.0", "linux", "arm64", "", "minion_0.1.0_linux_arm64.tar.gz"},
		{"v0.1.0", "linux", "arm", "7", "minion_0.1.0_linux_armv7.tar.gz"},
		{"v0.1.0", "darwin", "arm64", "", "minion_0.1.0_darwin_arm64.tar.gz"},
		{"v0.1.0", "windows", "amd64", "", "minion_0.1.0_windows_amd64_preview.zip"},
		{"v1.0.0-rc.1", "windows", "arm64", "", "minion_1.0.0-rc.1_windows_arm64_preview.zip"},
	}
	for _, tc := range cases {
		if got := ArchiveName(tc.tag, tc.os, tc.arch, tc.arm); got != tc.want {
			t.Errorf("ArchiveName(%q, %s, %s, %s) = %q, want %q", tc.tag, tc.os, tc.arch, tc.arm, got, tc.want)
		}
	}
	if goarmFor("arm") != "7" || goarmFor("amd64") != "" {
		t.Error("goarmFor")
	}
}
