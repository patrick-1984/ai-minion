package filexfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// RFC 8785 vectors restricted to the subset (no floating point numbers).
func TestJCSVectors(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			// RFC 8785 section 3.2.2 without the numbers member.
			"rfc8785 3.2.2 strings and literals",
			`{"string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/", "literals": [null, true, false]}`,
			"{\"literals\":[null,true,false],\"string\":\"\u20ac$\\u000f\\nA'B\\\"\\\\\\\\\\\"/\"}",
		},
		{
			// RFC 8785 section 3.2.3: sorting by UTF-16 code units.
			"rfc8785 3.2.3 key order",
			`{"\u20ac": "Euro Sign", "\r": "Carriage Return", "\ufb33": "Hebrew Letter Dalet With Dagesh", "1": "One",
			  "\ud83d\ude00": "Emoji: Grinning Face", "\u0080": "Control", "\u00f6": "Latin Small Letter O With Diaeresis"}`,
			"{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\"," +
				"\"\u20ac\":\"Euro Sign\",\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
		},
		{"integers", `[0, -1, 9007199254740991, -9007199254740991]`, `[0,-1,9007199254740991,-9007199254740991]`},
		{"nested", `{"b": [{"z": 1, "a": "x"}], "a": {}}`, `{"a":{},"b":[{"a":"x","z":1}]}`},
		{"control characters", `["\u0001\b\t\n\f\r\u001f\u007f\u2028"]`, "[\"\\u0001\\b\\t\\n\\f\\r\\u001f\u007f\u2028\"]"},
	}
	for _, c := range cases {
		got, err := CanonicalizeJSON([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Fatalf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		// The reference implementation agrees.
		ref, err := jsoncanonicalizer.Transform([]byte(c.in))
		if err != nil || !bytes.Equal(ref, got) {
			t.Fatalf("%s: reference %q (%v), ours %q", c.name, ref, err, got)
		}
	}
}

func TestJCSRefusesOutsideSubset(t *testing.T) {
	for _, in := range []string{`[1.5]`, `[1e3]`, `[9007199254740992]`, `{"a":1,"a":2}`, `[1] [2]`} {
		if _, err := CanonicalizeJSON([]byte(in)); err == nil {
			t.Fatalf("%s accepted", in)
		}
	}
	if _, err := Canonicalize("\xff"); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestManifestCanonicalMatchesReference(t *testing.T) {
	var m Manifest
	names := []string{"a", "b/\u00e9t\u00e9.txt", "c \"quoted\" name", "space name", "\u65e5\u672c"}
	for i, n := range names {
		sum := sha256.Sum256([]byte(n))
		typ, size, digest := proto.FileTypeFile, int64(i*1000+1), hex.EncodeToString(sum[:])
		if i == 0 {
			typ, size, digest = proto.FileTypeDir, 0, ""
		}
		if err := m.Add(proto.ManifestEntry{Path: n, Size: size, SHA256: digest, Type: typ}, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(m.Entries)
	ref, err := jsoncanonicalizer.Transform(plain)
	if err != nil || !bytes.Equal(ref, got) {
		t.Fatalf("canonical manifest differs from the reference:\n got %s\nwant %s", got, ref)
	}
	if int64(len(got)) != m.size {
		t.Fatalf("tracked size %d, canonical %d", m.size, len(got))
	}
	back, err := ParseManifest(got, DefaultLimits())
	if err != nil || len(back.Entries) != len(m.Entries) {
		t.Fatalf("parse: %v", err)
	}
	// Non-canonical forms of the same content are refused.
	for _, bad := range [][]byte{plain, append([]byte(" "), got...), bytes.Replace(got, []byte(`"size":1001,`), []byte(`"size":1001.0,`), 1)} {
		if _, err := ParseManifest(bad, DefaultLimits()); proto.CodeOf(err) != proto.CodeIntegrityFailed {
			t.Fatalf("non-canonical manifest: %v", err)
		}
	}
}

func TestManifestRefusesInvalidEntries(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, e := range []string{
		`[{"path":"../x","sha256":"` + sum + `","size":1,"type":"file"}]`,
		`[{"path":"a","sha256":"","size":1,"type":"file"}]`,
		`[{"path":"a","sha256":"` + sum + `","size":0,"type":"dir"}]`,
		`[{"path":"a","sha256":"","size":0,"type":"symlink"}]`,
		`[{"extra":1,"path":"a","sha256":"","size":0,"type":"dir"}]`,
		`[{"path":".minion/manifest.json","sha256":"` + sum + `","size":1,"type":"file"}]`,
	} {
		if _, err := ParseManifest([]byte(e), DefaultLimits()); err == nil {
			t.Fatalf("accepted %s", e)
		}
	}
	// The size limit.
	var m Manifest
	for i := 0; i < 100; i++ {
		if err := m.Add(proto.ManifestEntry{Path: fmt.Sprintf("d%03d", i), Type: proto.FileTypeDir}, 1000); err != nil {
			if proto.CodeOf(err) != proto.CodeTransferTooLarge {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("manifest limit not enforced")
}

func TestSingleFileManifest(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	m := SingleFileManifest("a.txt", 5, sum)
	b, _ := m.Canonical()
	want := `[{"path":"a.txt","sha256":"` + hex.EncodeToString(sum[:]) + `","size":5,"type":"file"}]`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	d, _ := m.Digest()
	if !proto.ValidManifestDigest(DigestHex(d)) {
		t.Fatal("digest form")
	}
}
