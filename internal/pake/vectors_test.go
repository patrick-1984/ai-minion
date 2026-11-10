package pake

import (
	"bufio"
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gtank/ristretto255"
)

// Official test vectors of draft-irtf-cfrg-cpace-21, Appendix A.1-A.3 and
// Appendix B.3 (CPACE-RISTR255-SHA512). The source is a verbatim excerpt of
// the draft in testdata; see the header of that file for provenance.
const vectorFile = "testdata/draft-irtf-cfrg-cpace-21-ristretto255.txt"

var headingRE = regexp.MustCompile(`^(Appendix )?[A-Z]\.[0-9.]*\s`)

// sectionLines returns the lines of one draft section (heading excluded),
// up to the next heading.
func sectionLines(t *testing.T, section string) []string {
	t.Helper()
	f, err := os.Open(vectorFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, section+"  ") {
			in = true
			continue
		}
		if in && (headingRE.MatchString(line) || strings.HasPrefix(line, "-----8<")) {
			break
		}
		if in {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !in {
		t.Fatalf("section %s not found in %s", section, vectorFile)
	}
	return out
}

// jsonBlock decodes a "Testvectors as JSON file encoded as BASE64" section.
func jsonBlock(t *testing.T, section string) map[string]any {
	t.Helper()
	var b64 strings.Builder
	for _, l := range sectionLines(t, section) {
		if s, ok := strings.CutPrefix(l, "    #"); ok {
			b64.WriteString(s)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		t.Fatalf("%s: base64: %v", section, err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: json: %v", section, err)
	}
	return m
}

// textHex reads the hex value printed under the first line of a section
// that contains label (the human-readable form of the vectors).
func textHex(t *testing.T, section, label string) []byte {
	t.Helper()
	isHex := regexp.MustCompile(`^[0-9a-f]+$`)
	var acc strings.Builder
	found := false
	for _, l := range sectionLines(t, section) {
		tl := strings.TrimSpace(l)
		if !found {
			found = strings.Contains(l, label)
			continue
		}
		if !isHex.MatchString(tl) {
			if acc.Len() > 0 {
				break
			}
			continue
		}
		acc.WriteString(tl)
	}
	if acc.Len() == 0 {
		t.Fatalf("%s: no hex under %q", section, label)
	}
	return mustHex(t, acc.String())
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ToLower(s))
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func hexField(t *testing.T, m map[string]any, key string) []byte {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("vector key %q missing", key)
	}
	return mustHex(t, v)
}

func eq(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n got  %x\n want %x", what, got, want)
	}
}

// --- transcript_oc helpers (symmetric setting, A.3.1-A.3.6). The protocol
// in this package uses transcript_ir; these exist to check the draft's
// parallel-execution vectors (B.3.6, B.3.7).

func lexicographicallyLarger(a, b []byte) bool {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return len(a) > len(b)
}

func oCat(a, b []byte) []byte {
	out := []byte("oc")
	if lexicographicallyLarger(a, b) {
		return append(append(out, a...), b...)
	}
	return append(append(out, b...), a...)
}

func transcriptOC(ya, ada, yb, adb []byte) []byte {
	return oCat(lvCat(ya, ada), lvCat(yb, adb))
}

func rangeBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// A.1.2 / A.1.2.1
func TestVectorPrependLen(t *testing.T) {
	m := jsonBlock(t, "A.1.2.1.")
	eq(t, "prepend_len(b'')", prependLen(nil), hexField(t, m, "prepend_len(b'')"))
	eq(t, "prepend_len(b'1234')", prependLen([]byte("1234")), hexField(t, m, "prepend_len(b'1234')"))
	eq(t, "prepend_len(range(127))", prependLen(rangeBytes(127)), hexField(t, m, "prepend_len(bytes(range(127)))"))
	eq(t, "prepend_len(range(128))", prependLen(rangeBytes(128)), hexField(t, m, "prepend_len(bytes(range(128)))"))
	// Cross-check against the human-readable listing in A.1.2.
	eq(t, "A.1.2 text", prependLen(rangeBytes(128)), textHex(t, "A.1.2.", "prepend_len(bytes(range(128)))"))
}

// A.1.4 / A.1.4.1
func TestVectorLvCat(t *testing.T) {
	m := jsonBlock(t, "A.1.4.1.")
	got := lvCat([]byte("1234"), []byte("5"), nil, []byte("678"))
	eq(t, "lv_cat", got, hexField(t, m, "lv_cat(b'1234',b'5',b'',b'678')"))
}

// A.3.3 / A.3.3.1
func TestVectorOrderedConcat(t *testing.T) {
	// Transcribed from the list in A.3.3.
	cases := []struct {
		a, b string
		want bool
	}{
		{"\x00", "\x00\x00", false},
		{"\x01", "\x00\x00", true},
		{"\x00\x00", "\x00", true},
		{"\x00\x00", "\x01", false},
		{"\x00\x01", "\x01", false},
		{"ABCD", "BCD", false},
	}
	for _, c := range cases {
		if got := lexicographicallyLarger([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("lexiographically_larger(%q,%q) = %v", c.a, c.b, got)
		}
	}
	m := jsonBlock(t, "A.3.3.1.")
	eq(t, "o_cat(ABCD,BCD)", oCat([]byte("ABCD"), []byte("BCD")), hexField(t, m, "o_cat(b'ABCD',b'BCD')"))
	eq(t, "o_cat(BCD,ABCDE)", oCat([]byte("BCD"), []byte("ABCDE")), hexField(t, m, "o_cat(b'BCD',b'ABCDE')"))
}

// A.3.5.1 and A.3.7.1
func TestVectorTranscripts(t *testing.T) {
	ir := jsonBlock(t, "A.3.5.1.")
	oc := jsonBlock(t, "A.3.7.1.")
	pa, pb := []byte("PartyA"), []byte("PartyB")
	eq(t, "transcript_ir 1", transcriptIR([]byte("123"), pa, []byte("234"), pb),
		hexField(t, ir, "transcript_ir(b'123',b'PartyA',b'234',b'PartyB')"))
	eq(t, "transcript_ir 2", transcriptIR([]byte("3456"), pa, []byte("2345"), pb),
		hexField(t, ir, "transcript_ir(b'3456',b'PartyA',b'2345',b'PartyB')"))
	eq(t, "transcript_oc 1", transcriptOC([]byte("123"), pa, []byte("234"), pb),
		hexField(t, oc, "transcript_oc(b'123',b'PartyA',b'234',b'PartyB')"))
	eq(t, "transcript_oc 2", transcriptOC([]byte("3456"), pa, []byte("2345"), pb),
		hexField(t, oc, "transcript_oc(b'3456',b'PartyA',b'2345',b'PartyB')"))
}

// B.3.1 / B.3.1.1
func TestVectorCalculateGenerator(t *testing.T) {
	m := jsonBlock(t, "B.3.1.1.")
	if m["H"] != "SHA-512" || m["H.s_in_bytes"] != float64(hashBlockSize) {
		t.Fatalf("unexpected suite in vector: %v %v", m["H"], m["H.s_in_bytes"])
	}
	eq(t, "DSI", []byte(DSI), hexField(t, m, "DSI"))
	prs, ci, sid := hexField(t, m, "PRS"), hexField(t, m, "CI"), hexField(t, m, "sid")

	gs := generatorString([]byte(DSI), prs, ci, sid, hashBlockSize)
	eq(t, "generator_string", gs, hexField(t, m, "generator_string(G.DSI,PRS,CI,sid,H.s_in_bytes)"))
	zpad := hashBlockSize - 1 - len(prependLen(prs)) - len(prependLen([]byte(DSI)))
	if want := m["ZPAD length"].(float64); float64(zpad) != want {
		t.Errorf("ZPAD length %d, want %v", zpad, want)
	}
	h := sha512.Sum512(gs)
	eq(t, "hash result", h[:], hexField(t, m, "hash result"))
	eq(t, "encoded generator g", calculateGenerator(prs, ci, sid).Bytes(), hexField(t, m, "encoded generator g"))
	eq(t, "B.3.1 text g", calculateGenerator(prs, ci, sid).Bytes(), textHex(t, "B.3.1.", "encoded generator g"))
}

func scalarFromLE(t *testing.T, b []byte) *ristretto255.Scalar {
	t.Helper()
	s, err := ristretto255.NewScalar().SetCanonicalBytes(b)
	if err != nil {
		t.Fatalf("scalar %x: %v", b, err)
	}
	return s
}

// B.3.2-B.3.9: full protocol run with the draft's fixed scalars, through
// the package's real code path (start/respond/finish).
func TestVectorProtocol(t *testing.T) {
	m := jsonBlock(t, "B.3.9.")
	prs, ci, sid := hexField(t, m, "PRS"), hexField(t, m, "CI"), hexField(t, m, "sid")
	ada, adb := hexField(t, m, "ADa"), hexField(t, m, "ADb")
	eq(t, "ADa", ada, []byte("ADa"))
	eq(t, "ADb", adb, []byte("ADb"))
	ya, yb := hexField(t, m, "ya"), hexField(t, m, "yb")
	wantYa, wantYb, wantK := hexField(t, m, "Ya"), hexField(t, m, "Yb"), hexField(t, m, "K")
	wantISK := hexField(t, m, "ISK_IR")

	eq(t, "g", calculateGenerator(prs, ci, sid).Bytes(), hexField(t, m, "g"))

	init, msgA, err := startWithScalar(Params{Password: prs, SessionID: sid, ChannelID: ci, AD: ada}, scalarFromLE(t, ya))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "Ya", msgA[:ElementSize], wantYa)
	eq(t, "message A", msgA, append(clone(wantYa), ada...))

	resp, msgB, err := respondWithScalar(Params{Password: prs, SessionID: sid, ChannelID: ci, AD: adb}, msgA, scalarFromLE(t, yb))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "Yb", msgB[:ElementSize], wantYb)

	kA, err := scalarMultVfy(scalarFromLE(t, ya), wantYb)
	if err != nil {
		t.Fatal(err)
	}
	kB, err := scalarMultVfy(scalarFromLE(t, yb), wantYa)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "K = scalar_mult_vfy(ya,Yb)", kA, wantK)
	eq(t, "K = scalar_mult_vfy(yb,Ya)", kB, wantK)

	sessA, err := init.Finish(msgB)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "ISK_IR (initiator)", sessA.Key(), wantISK)
	eq(t, "ISK_IR (responder)", resp.Key(), wantISK)
	eq(t, "B.3.5 text ISK", sessA.Key(), textHex(t, "B.3.5.", "ISK result"))
	eq(t, "B.3.5 transcript_ir", transcriptIR(wantYa, ada, wantYb, adb), textHex(t, "B.3.5.", "transcript_ir(Ya,ADa,Yb,ADb)"))
	eq(t, "B.3.5 ISK input", append(lvCat([]byte(DSI+"_ISK"), sid, wantK), transcriptIR(wantYa, ada, wantYb, adb)...),
		textHex(t, "B.3.5.", "lv_cat(DSI,sid,K)||transcript_ir"))

	// B.3.6: parallel (symmetric) setting.
	hOC := sha512.New()
	hOC.Write(lvCat([]byte(DSI+"_ISK"), sid, wantK))
	hOC.Write(transcriptOC(wantYa, ada, wantYb, adb))
	eq(t, "ISK_SY", hOC.Sum(nil), hexField(t, m, "ISK_SY"))

	// B.3.7: optional sid output.
	so := sha512.Sum512(append([]byte("CPaceSidOutput"), transcriptIR(wantYa, ada, wantYb, adb)...))
	eq(t, "sid_output_ir", so[:], hexField(t, m, "sid_output_ir"))
	so = sha512.Sum512(append([]byte("CPaceSidOutput"), transcriptOC(wantYa, ada, wantYb, adb)...))
	eq(t, "sid_output_oc", so[:], hexField(t, m, "sid_output_oc"))

	// Confirmation (10.4) on top of the vector run.
	if err := sessA.VerifyConfirmation(resp.Confirmation()); err != nil {
		t.Error(err)
	}
	if err := resp.VerifyConfirmation(sessA.Confirmation()); err != nil {
		t.Error(err)
	}
	eq(t, "responder sees ADa", resp.PeerAD(), ada)
	eq(t, "initiator sees ADb", sessA.PeerAD(), adb)
}

// B.3.10 / B.3.11 / B.3.11.1
func TestVectorScalarMultVfy(t *testing.T) {
	m := jsonBlock(t, "B.3.11.1.")
	valid := m["Valid"].(map[string]any)
	s := scalarFromLE(t, hexField(t, valid, "s"))
	x := hexField(t, valid, "X")

	X, err := ristretto255.NewElement().SetCanonicalBytes(x)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "scalar_mult(s,decode(X))", ristretto255.NewElement().ScalarMult(s, X).Bytes(), hexField(t, valid, "G.scalar_mult(s,decode(X))"))
	got, err := scalarMultVfy(s, x)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "scalar_mult_vfy(s,X)", got, hexField(t, valid, "G.scalar_mult_vfy(s,X)"))
	eq(t, "B.3.10 text", got, textHex(t, "B.3.10.", "G.scalar_mult_vfy(s,X)"))

	for _, key := range []string{"Invalid Y1", "Invalid Y2"} {
		y := hexField(t, m, key)
		if _, err := scalarMultVfy(s, y); err == nil {
			t.Errorf("%s: scalar_mult_vfy accepted %x", key, y)
		}
		// The protocol MUST abort when such a point is in a message.
		p := Params{Password: []byte("pw"), SessionID: []byte("sid")}
		if _, _, err := Respond(p, y); err == nil {
			t.Errorf("%s: Respond accepted it", key)
		}
		init, _, err := Start(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := init.Finish(y); err == nil {
			t.Errorf("%s: Finish accepted it", key)
		}
	}
	eq(t, "G.I", identityEncoding, hexField(t, m, "Invalid Y2"))
}
