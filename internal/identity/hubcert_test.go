package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
	"time"
)

type certVector struct {
	Name      string `json:"name"`
	RootSeed  string `json:"root_seed_hex"`
	OpSeed    string `json:"op_seed_hex"`
	Payload   string `json:"payload_hex"`
	Signature string `json:"signature_hex"`
	Cert      string `json:"cert_b64url"`
	Sha       string `json:"cert_sha256_hex"`
	Expect    string `json:"expect"`
	NowMs     *int64 `json:"now_ms"`
	Note      string `json:"note"`
}

func loadCertVectors(t *testing.T) []certVector {
	t.Helper()
	b, err := os.ReadFile("testdata/hub_cert_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []certVector
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatal(err)
	}
	return vs
}

func seedKey(t *testing.T, h string) ed25519.PrivateKey {
	t.Helper()
	s, err := hex.DecodeString(h)
	if err != nil || len(s) != 32 {
		t.Fatalf("bad seed %q", h)
	}
	return ed25519.NewKeyFromSeed(s)
}

// Literal values of design section 5.1, independent of the JSON file.
const (
	specRootPub    = "03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8"
	specOpPub      = "cd14b37f956e953194ff7fb73b3d81dcc561d61a7538094b7c3e1a643ee5f3aa"
	specTypicalB64 = "AQOhB7_zzhC-HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4zRSzf5VulTGU_3-3Oz2B3MVh1hp1OAlLfD4aZD7l86oAAAGTGIUoAAAAAZMYhSgAAAABlOgBgABWq2b9APhWcZWeQtPuMWEV4IffPYUS899fNNPflxhPErgQEnhWG5v6toIvNZNpvbt0FMD1mVeyNnh6cLY40gcA"
	specTypicalSHA = "958750b172672c290adec122358e6639aa506fe2077e5fee58d523678c819e31"
	specExtremeB64 = "AQOhB7_zzhC-HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4zRSzf5VulTGU_3-3Oz2B3MVh1hp1OAlLfD4aZD7l86p__________wAAAAAAAAAAAAAACAvvwAC9gth1BIP__KBIdM-pGVIzUKm5t8KWW1y3AG7fVQChmTrTcroADMJEDIQyrPwEBmtTfu-lG6Qdz9YUtne-PasJ"
	specExtremeSHA = "71773984ff4998e4589feed80f5ae794203f92dbb44d5ddac340afbc50e52517"
)

func TestHubCertSpecLiterals(t *testing.T) {
	var typ, ext *certVector
	vs := loadCertVectors(t)
	for i := range vs {
		switch vs[i].Name {
		case "typical":
			typ = &vs[i]
		case "extremes":
			ext = &vs[i]
		}
	}
	if typ == nil || ext == nil {
		t.Fatal("missing positive vectors")
	}
	if typ.Cert != specTypicalB64 || typ.Sha != specTypicalSHA || ext.Cert != specExtremeB64 || ext.Sha != specExtremeSHA {
		t.Fatal("testdata differs from the specification")
	}
	root := seedKey(t, typ.RootSeed)
	op := seedKey(t, typ.OpSeed)
	if hex.EncodeToString(root.Public().(ed25519.PublicKey)) != specRootPub || hex.EncodeToString(op.Public().(ed25519.PublicKey)) != specOpPub {
		t.Fatal("public keys differ from the specification")
	}
}

// Decoder direction: every vector gives its expected first failure.
func TestHubCertVectorsParseVerify(t *testing.T) {
	for _, v := range loadCertVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			raw, err := base64.RawURLEncoding.DecodeString(v.Cert)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != v.Sha {
				t.Fatal("sha256 field wrong")
			}
			rootPub := seedKey(t, v.RootSeed).Public().(ed25519.PublicKey)
			got := reasonOf(rawCheck(raw, rootPub, v.NowMs))
			if got != v.Expect {
				t.Fatalf("got %q, want %q", got, v.Expect)
			}
		})
	}
}

func rawCheck(raw []byte, root ed25519.PublicKey, now *int64) error {
	c, err := ParseHubCert(raw)
	if err != nil {
		return err
	}
	if err := c.Verify(root); err != nil {
		return err
	}
	if now != nil {
		return c.CheckWindow(*now)
	}
	return nil
}

func reasonOf(err error) string {
	if err == nil {
		return "ok"
	}
	var ce *CertError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return "other:" + err.Error()
}

// Encoder direction: fields read from the payload by hand, signed again.
func TestHubCertVectorsEncode(t *testing.T) {
	for _, v := range loadCertVectors(t) {
		if v.Expect != "ok" || v.Payload == "" {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			p, _ := hex.DecodeString(v.Payload)
			root := seedKey(t, v.RootSeed)
			c, err := IssueHubCert(root, p[33:65], binary.BigEndian.Uint64(p[65:73]),
				int64(binary.BigEndian.Uint64(p[73:81])), int64(binary.BigEndian.Uint64(p[81:89])))
			if err != nil {
				t.Fatal(err)
			}
			if c.Encode() != v.Cert || hex.EncodeToString(c.Payload()) != v.Payload || hex.EncodeToString(c.Signature) != v.Signature {
				t.Fatal("encoded certificate differs from vector")
			}
			sum := c.SHA256()
			if hex.EncodeToString(sum[:]) != v.Sha {
				t.Fatal("SHA256 differs")
			}
			if len(c.Encode()) != 204 || len(c.Bytes()) != HubCertSize {
				t.Fatal("size")
			}
			c2, err := ParseHubCertB64(v.Cert)
			if err != nil || c2.Serial != c.Serial {
				t.Fatal(err)
			}
			pc, err := ParseHubCertPayload(p)
			if err != nil || pc.Signature != nil || pc.Bytes() != nil {
				t.Fatal("payload parse")
			}
		})
	}
}

func TestHubCertPayloadParseRejections(t *testing.T) {
	if _, err := ParseHubCertPayload(make([]byte, 88)); !errors.Is(err, ErrCertMalformed) {
		t.Fatal(err)
	}
	if _, err := ParseHubCertB64("!!"); !errors.Is(err, ErrCertMalformed) {
		t.Fatal(err)
	}
}

func TestHubCertIssueRejectsInvalid(t *testing.T) {
	root := seedKey(t, hex.EncodeToString(make([]byte, 32)))
	op := seedKey(t, hex.EncodeToString(append(make([]byte, 31), 1))).Public().(ed25519.PublicKey)
	cases := []struct {
		name   string
		op     ed25519.PublicKey
		serial uint64
		nb, na int64
	}{
		{"same key", root.Public().(ed25519.PublicKey), 1, 0, 10},
		{"short op", op[:31], 1, 0, 10},
		{"serial 0", op, 0, 0, 10},
		{"serial too big", op, 1 << 63, 0, 10},
		{"negative nb", op, 1, -1, 10},
		{"min na", op, 1, 1, math.MinInt64},
		{"too long", op, 1, 0, MaxHubCertValidityMs + 1},
	}
	for _, c := range cases {
		if _, err := IssueHubCert(root, c.op, c.serial, c.nb, c.na); !errors.Is(err, ErrCertMalformed) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if _, err := IssueHubCert(root[:10], op, 1, 0, 10); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("short root key")
	}
	if _, err := IssueHubCert(root, op, 1, 0, MaxHubCertValidityMs); err != nil {
		t.Fatal(err)
	}
}

func TestHubCertWrongRoot(t *testing.T) {
	root := seedKey(t, hex.EncodeToString(make([]byte, 32)))
	other, _ := GenerateKey()
	op, _ := GenerateKey()
	c, err := IssueHubCert(root, op.Public(), 5, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(other.Public()); !errors.Is(err, ErrCertRootMismatch) {
		t.Fatal(err)
	}
	if err := c.Verify(nil); !errors.Is(err, ErrCertRootMismatch) {
		t.Fatal(err)
	}
	// A cert that names the right root but was signed by another key.
	forged := *c
	sig, _ := other.Sign(PurposeHubCert, c.Payload())
	forged.Signature = sig
	if err := forged.Verify(root.Public().(ed25519.PublicKey)); !errors.Is(err, ErrCertBadSignature) {
		t.Fatal(err)
	}
	unsigned := *c
	unsigned.Signature = nil
	if err := unsigned.Verify(root.Public().(ed25519.PublicKey)); !errors.Is(err, ErrCertBadSignature) {
		t.Fatal(err)
	}
	if err := c.Verify(root.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
}

func TestHubCertPurposeAppliedOnce(t *testing.T) {
	if !PurposeHubCert.Valid() {
		t.Fatal("hub-cert not valid")
	}
	root, _ := GenerateKey()
	op, _ := GenerateKey()
	c, _ := IssueHubCert(ed25519.PrivateKey(root.key()), op.Public(), 1, 0, 1000)
	msg, _ := SignedMessage(PurposeHubCert, c.Payload())
	if !ed25519.Verify(root.Public(), msg, c.Signature) {
		t.Fatal("signature is not over prefix||0||payload")
	}
}

func TestCheckedSub(t *testing.T) {
	if _, ok := subChecked(math.MinInt64, 1); ok {
		t.Fatal("underflow accepted")
	}
	if _, ok := subChecked(math.MaxInt64, -1); ok {
		t.Fatal("overflow accepted")
	}
	if d, ok := subChecked(5, 7); !ok || d != -2 {
		t.Fatal("plain")
	}
	if d, ok := subChecked(math.MinInt64, math.MinInt64); !ok || d != 0 {
		t.Fatal("min-min")
	}
}

func TestCheckWindowBoundaries(t *testing.T) {
	c := &HubCert{NotBeforeMs: 10_000_000, NotAfterMs: 20_000_000}
	cases := []struct {
		now  int64
		want error
	}{
		{c.NotBeforeMs - HubCertToleranceMs - 1, ErrCertNotYetValid},
		{c.NotBeforeMs - HubCertToleranceMs, nil},
		{c.NotBeforeMs, nil},
		{c.NotAfterMs, nil},
		{c.NotAfterMs + 1, ErrCertExpired},
	}
	for _, k := range cases {
		if err := c.CheckWindow(k.now); (err != nil || k.want != nil) && !errors.Is(err, k.want) {
			t.Errorf("now=%d: %v want %v", k.now, err, k.want)
		}
	}
	// not_before - 1h would underflow: clamps, never wraps.
	w := &HubCert{NotBeforeMs: math.MinInt64 + 5, NotAfterMs: 0}
	if err := w.CheckWindow(math.MinInt64 + 1); err != nil {
		t.Fatal(err)
	}
}

func TestMonotonicDeadline(t *testing.T) {
	now := time.Now()
	c := &HubCert{NotAfterMs: now.UnixMilli() + 5000}
	d, err := c.MonotonicDeadline(now)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Sub(now); got != 5*time.Second {
		t.Fatalf("remaining %v", got)
	}
	if !d.After(now) || !hasMonotonic(d) { // d must keep the monotonic reading
		t.Fatal("monotonic reading lost")
	}
	c.NotAfterMs = now.UnixMilli()
	if d, err := c.MonotonicDeadline(now); err != nil || d.UnixMilli() != now.UnixMilli() {
		t.Fatal("exactly at not_after")
	}
	c.NotAfterMs = now.UnixMilli() - 1
	if _, err := c.MonotonicDeadline(now); !errors.Is(err, ErrCertExpired) {
		t.Fatal(err)
	}
	c.NotAfterMs = math.MaxInt64
	if _, err = c.MonotonicDeadline(now); !errors.Is(err, ErrCertMalformed) {
		t.Fatalf("unrepresentable horizon: %v", err)
	}
	// Whenever a deadline is returned it keeps the monotonic reading.
	for _, years := range []int64{1, 50, 100, 130, 140, 200, 291, 300} {
		c.NotAfterMs = now.UnixMilli() + years*365*24*3600*1000
		d, err := c.MonotonicDeadline(now)
		if err != nil {
			if !errors.Is(err, ErrCertMalformed) {
				t.Fatalf("%d years: %v", years, err)
			}
			continue
		}
		if !hasMonotonic(d) {
			t.Fatalf("%d years: monotonic reading lost", years)
		}
	}
	if _, err := (&HubCert{NotAfterMs: math.MaxInt64}).MonotonicDeadline(time.UnixMilli(-1)); !errors.Is(err, ErrCertMalformed) {
		t.Fatal("overflow must be rejected")
	}
}
