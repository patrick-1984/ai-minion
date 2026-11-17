package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestSPKIPinIsTheCurlPinnedPubkeyForm(t *testing.T) {
	// SHA-256 of the empty input, standard base64 with padding.
	if got := SPKIPin(nil); got != "sha256//47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=" {
		t.Fatalf("SPKIPin(nil) = %q", got)
	}
}

func TestParseSPKIPins(t *testing.T) {
	a, b := SPKIPin(nil), SPKIPin([]byte("x"))
	got, err := ParseSPKIPins(" " + a + " ;" + b)
	if err != nil || len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, v := range []string{"", ";", a + ";", "CANARY", "sha256//", "sha256//AAAA", strings.Replace(a, "=", "", 1), "SHA256//" + strings.TrimPrefix(a, "sha256//")} {
		_, err := ParseSPKIPins(v)
		if !errors.Is(err, ErrSPKIPin) || strings.Contains(err.Error(), "CANARY") {
			t.Errorf("%q: %v", v, err)
		}
	}
}
