package identity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// logAll logs v with both slog handlers and returns the combined output.
func logAll(v any) string {
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("m", "v", v, slog.Any("grouped", v))
	slog.New(slog.NewTextHandler(&buf, nil)).Info("m", "v", v)
	return buf.String()
}

func mustNotContain(t *testing.T, what, out string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			t.Fatalf("%s: empty secret in test", what)
		}
		if strings.Contains(out, s) {
			t.Errorf("%s leaks %q in %q", what, s, out)
		}
	}
}

func TestKeyNeverPrintsSecret(t *testing.T) {
	seed := bytes.Repeat([]byte{0x5a}, 32)
	k, err := KeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{
		EncodeB64(seed), fmt.Sprintf("%x", seed), fmt.Sprintf("%v", seed),
		fmt.Sprintf("%x", []byte(k.key())), fmt.Sprintf("%v", []byte(k.key())),
	}
	fpr := k.Fingerprint().String()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d", "%q"} {
		out := fmt.Sprintf(verb, k)
		mustNotContain(t, "fmt "+verb, out, secrets...)
		if verb == "%v" || verb == "%+v" || verb == "%s" {
			if out != fpr {
				t.Errorf("%s = %q, want the fingerprint", verb, out)
			}
		}
	}
	// A key embedded in a struct is printed through the same methods.
	type holder struct{ K *Key }
	mustNotContain(t, "fmt struct", fmt.Sprintf("%+v", holder{k}), secrets...)

	out := logAll(k)
	mustNotContain(t, "slog", out, secrets...)
	if !strings.Contains(out, fpr) {
		t.Errorf("slog output has no fingerprint: %s", out)
	}
	if got := k.String(); got != fpr {
		t.Errorf("String = %q", got)
	}
}

func TestNilKeyPrints(t *testing.T) {
	var k *Key
	if got := fmt.Sprintf("%v", k); got != "<nil>" {
		t.Errorf("got %q", got)
	}
}

func TestInviteKeyLogRedacted(t *testing.T) {
	hub := mustKey(t)
	k, err := NewInviteKey(hub.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	out := logAll(k)
	mustNotContain(t, "slog", out, encodeB32(k.Secret), k.String(), EncodeB64(k.Secret), fmt.Sprintf("%v", k.Secret))
	if !strings.Contains(out, "mnk_"+string(k.Fingerprint)+".[redacted]") {
		t.Errorf("unexpected output: %s", out)
	}
	// The display method is unchanged.
	if !strings.HasSuffix(k.String(), encodeB32(k.Secret)) {
		t.Error("String changed")
	}
	// A pointer is redacted as well.
	mustNotContain(t, "slog ptr", logAll(&k), encodeB32(k.Secret))
}

func TestAPIKeyLogRedacted(t *testing.T) {
	k := NewAPIKey()
	body := strings.TrimPrefix(string(k), "mn_")
	out := logAll(k)
	mustNotContain(t, "slog", out, string(k), body[APIKeyHintLen:])
	if !strings.Contains(out, "mn_"+k.Hint()+"...[redacted]") {
		t.Errorf("unexpected output: %s", out)
	}
	if k.String() != string(k) {
		t.Error("String changed")
	}
}

func TestPairingCodeLogRedacted(t *testing.T) {
	c := NewPairingCode()
	out := logAll(c)
	mustNotContain(t, "slog", out, c.Canonical(), c.String(), c.Canonical()[4:])
	if !strings.Contains(out, "MN-"+c.PairingID()+"-[redacted]") {
		t.Errorf("unexpected output: %s", out)
	}
	if !strings.Contains(logAll(PairingCode{}), "[redacted]") {
		t.Error("zero code not redacted")
	}
	if c.String() != "MN-"+c.Canonical()[:4]+"-"+c.Canonical()[4:] {
		t.Error("String changed")
	}
}

// handlerOutputs logs v through the Text and JSON slog handlers separately.
func handlerOutputs(v any) map[string]string {
	var text, js bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("m", "v", v, slog.Group("g", slog.Any("k", v)))
	slog.New(slog.NewJSONHandler(&js, nil)).Info("m", "v", v, slog.Group("g", slog.Any("k", v)))
	return map[string]string{"text": text.String(), "json": js.String()}
}

// A Key VALUE, a pointer, and a Key nested in a struct, slice or map must not
// leak the private bytes through any fmt verb, slog handler or encoder.
func TestKeyValueNeverPrintsSecret(t *testing.T) {
	seed := bytes.Repeat([]byte{0x5a}, 32)
	kp, err := KeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	kv := *kp
	secrets := []string{
		EncodeB64(seed), fmt.Sprintf("%x", seed), fmt.Sprintf("%v", seed), "5a5a5a5a",
		fmt.Sprintf("%x", []byte(kv.key())), fmt.Sprintf("%v", []byte(kv.key())),
		fmt.Sprintf("%d", seed[:4]),
	}
	fpr := kp.Fingerprint().String()
	type holder struct {
		V Key
		P *Key
	}
	// Unexported fields: fmt cannot call their methods and reflects instead.
	type hidden struct {
		k Key
		p *Key
	}
	values := map[string]any{
		"unexported":         hidden{k: kv, p: kp},
		"unexported pointer": &hidden{k: kv, p: kp},
		"value":              kv,
		"pointer":            kp,
		"struct":             holder{V: kv, P: kp},
		"slice":              []Key{kv},
		"map":                map[string]Key{"k": kv},
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%p", "%08.3v", "%c", "%e", "%U", "%o", "%b", "%f", "%g"} {
			mustNotContain(t, name+" fmt "+verb, fmt.Sprintf(verb, v), secrets...)
		}
		for hname, out := range handlerOutputs(v) {
			mustNotContain(t, name+" slog "+hname, out, secrets...)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mustNotContain(t, name+" json", string(raw), secrets...)
	}
	// Direct values print the fingerprint.
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(verb, kv); got != fpr {
			t.Errorf("%s of a Key value = %q, want %q", verb, got, fpr)
		}
		if got := fmt.Sprintf(verb, kp); got != fpr {
			t.Errorf("%s of a *Key = %q, want %q", verb, got, fpr)
		}
	}
	for hname, out := range handlerOutputs(kv) {
		if !strings.Contains(out, fpr) {
			t.Errorf("slog %s of a Key value has no fingerprint: %s", hname, out)
		}
	}
	if kv.LogValue().String() != fpr || kv.GoString() != fpr || kv.String() != fpr {
		t.Error("value methods do not print the fingerprint")
	}
	if got := fmt.Sprint(Key{}); got == "" || strings.Contains(got, "[") {
		t.Errorf("zero Key prints %q", got)
	}
}

// Dumping the secret types with %#v, %d or %x must not reveal the secret.
func TestSecretTypesDumpRedacted(t *testing.T) {
	hub := mustKey(t)
	ik, err := NewInviteKey(hub.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	ak := NewAPIKey()
	pc := NewPairingCode()
	type holder struct {
		I InviteKey
		A APIKey
		P PairingCode
	}
	h := holder{ik, ak, pc}
	secrets := []string{
		encodeB32(ik.Secret), fmt.Sprintf("%x", ik.Secret), fmt.Sprintf("%v", ik.Secret),
		strings.TrimPrefix(string(ak), "mn_")[APIKeyHintLen:], fmt.Sprintf("%x", string(ak)),
		pc.Canonical()[4:], fmt.Sprintf("%x", pc.Canonical()),
	}
	for _, verb := range []string{"%#v", "%d", "%x", "%X", "%o", "%b"} {
		for name, v := range map[string]any{"invite": ik, "api": ak, "pair": pc, "holder": h, "invite ptr": &ik} {
			mustNotContain(t, name+" "+verb, fmt.Sprintf(verb, v), secrets...)
		}
	}
	// The display forms still work for %s and %v on a direct value.
	if fmt.Sprintf("%s", ik) != ik.String() || fmt.Sprintf("%v", ak) != ak.String() || fmt.Sprintf("%s", pc) != pc.String() {
		t.Error("display form changed")
	}
	if fmt.Sprintf("%q", pc) != `"`+pc.String()+`"` {
		t.Error("quoted display form changed")
	}
}
