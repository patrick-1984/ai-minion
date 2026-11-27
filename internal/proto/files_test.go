package proto

import (
	"crypto/sha256"
	"slices"
	"strings"
	"testing"
)

func TestFilesFeatureIsAnnounceable(t *testing.T) {
	if FeatureFilesV1 != "files.v1" {
		t.Fatalf("FeatureFilesV1 = %q: the name is wire format", FeatureFilesV1)
	}
	if got := NormaliseFeatures([]string{FeatureFilesV1}); !slices.Equal(got, []string{FeatureFilesV1}) {
		t.Fatalf("NormaliseFeatures = %v", got)
	}
}

// The data-leg stream kind is reserved but not accepted yet, so nothing about
// the existing stream handling changes before the transport gates it.
func TestFilesStreamKindReserved(t *testing.T) {
	if StreamFiles != "fx" || len(StreamFiles) != StreamKindPrefixLen {
		t.Fatalf("StreamFiles = %q", StreamFiles)
	}
	if _, ok := ParseStreamKind([]byte("fx")); ok {
		t.Fatal("fx is accepted before the transport gates it on files.v1")
	}
}

func TestFilesLegPath(t *testing.T) {
	id := "01890a5d-ac96-774b-bcce-b302099a8057"
	p := FilesLegPath(id)
	if p != "/files/v1/transfers/"+id+"/content" {
		t.Fatalf("FilesLegPath = %q", p)
	}
	if got, ok := ParseFilesLegPath(p); !ok || got != id {
		t.Fatalf("ParseFilesLegPath = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "/files/v1/transfers//content", "/files/v1/transfers/a/b/content",
		"/files/v1/transfers/a", "/files/v2/transfers/a/content", "/files/v1/transfers/a%2fb/content",
		"/files/v1/transfers/a/content?x", "/files/v1/transfers/a/content/"} {
		if _, ok := ParseFilesLegPath(bad); ok {
			t.Errorf("ParseFilesLegPath(%q) accepted", bad)
		}
	}
}

func TestLegToken(t *testing.T) {
	a, err := NewLegToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewLegToken()
	if len(a) != LegTokenLen || !ValidLegToken(a) || a == b {
		t.Fatalf("tokens %q %q", a, b)
	}
	if !LegTokenEqual(a, a) || LegTokenEqual(a, b) || LegTokenEqual("", "") || LegTokenEqual("x", "x") {
		t.Error("LegTokenEqual")
	}
	for _, bad := range []string{"", a[:LegTokenLen-1], a + "A", strings.Repeat("+", LegTokenLen), a[:LegTokenLen-1] + "="} {
		if ValidLegToken(bad) {
			t.Errorf("ValidLegToken(%q)", bad)
		}
	}
}

func TestDigestField(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	f := FormatDigestField(sum[:])
	if f != "sha-256=:ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=:" {
		t.Fatalf("FormatDigestField = %q", f)
	}
	for _, v := range []string{f, "sha-512=:AAAA:, " + f} {
		if got, ok := ParseDigestField(v); !ok || string(got) != string(sum[:]) {
			t.Errorf("ParseDigestField(%q) = %x, %v", v, got, ok)
		}
	}
	for _, bad := range []string{"", "sha-256=abc", "sha-256=:AAAA:", "sha-512=:" + f[9:]} {
		if _, ok := ParseDigestField(bad); ok {
			t.Errorf("ParseDigestField(%q) accepted", bad)
		}
	}
	if !ValidManifestDigest(strings.Repeat("0f", 32)) || ValidManifestDigest(strings.Repeat("0F", 32)) || ValidManifestDigest("ab") {
		t.Error("ValidManifestDigest")
	}
}

func TestFilesLimits(t *testing.T) {
	if MaxTransferBytes != 4<<30 || MaxTransferEntries != 100000 || MaxPathDepth != 32 || MaxPathBytes != 4096 ||
		MaxNameBytes != 255 || MaxFileControlResult != 48<<10 || FileListDefaultLimit != 200 || FileListMaxLimit != 500 {
		t.Fatal("a limit differs from the BRD")
	}
	if got := MaxWireBytes(1000); got != 1050+16<<20 {
		t.Errorf("MaxWireBytes(1000) = %d", got)
	}
	if got := MaxWireBytes(MaxTransferBytes); got != MaxTransferBytes*1050/1000+16<<20 {
		t.Errorf("MaxWireBytes(4 GiB) = %d", got)
	}
	if MaxWireBytes(-1) != WireSlackBytes || MaxDecodedBytes(10, 2) != 10+2*4096 || MaxDecodedBytes(-1, -1) != 0 {
		t.Error("bounds of negative input")
	}
	for _, e := range []string{EncodingZstd, EncodingGzip, EncodingIdentity} {
		if !ValidEncoding(e) {
			t.Errorf("ValidEncoding(%q)", e)
		}
	}
	if ValidEncoding("br") {
		t.Error("br is not a files.v1 coding")
	}
}
