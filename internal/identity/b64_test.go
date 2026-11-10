package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"testing/quick"
)

func TestB64RoundTripProperty(t *testing.T) {
	err := quick.Check(func(b []byte) bool {
		s := EncodeB64(b)
		d, err := DecodeB64(s)
		return err == nil && bytes.Equal(d, b)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestB64Format(t *testing.T) {
	// 0xfb 0xff encodes to "-_8" in base64url and "+/8=" in standard base64.
	if got := EncodeB64([]byte{0xfb, 0xff}); got != "-_8" {
		t.Fatalf("got %q", got)
	}
	// CR and LF in an otherwise valid encoding are rejected too: Go's decoder
	// alone would skip them.
	for _, bad := range []string{"-_8=", "+/8", "-_9", "a b", "a\n", "-_8\n", "-_8\r", "-\r_8", "-_\n8", "\n-_8", "-_8\r\n"} {
		if _, err := DecodeB64(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestB64JSON(t *testing.T) {
	type s struct {
		K B64 `json:"k"`
	}
	raw, err := json.Marshal(s{K: B64{0xfb, 0xff}})
	if err != nil || string(raw) != `{"k":"-_8"}` {
		t.Fatalf("%s %v", raw, err)
	}
	var out s
	if err := json.Unmarshal(raw, &out); err != nil || !bytes.Equal(out.K, []byte{0xfb, 0xff}) {
		t.Fatalf("%v %v", out, err)
	}
	if err := json.Unmarshal([]byte(`{"k":"-_8="}`), &out); err == nil {
		t.Fatal("padding accepted")
	}
}

func TestB64JSONRejectsLineBreaks(t *testing.T) {
	var out B64
	for _, in := range []string{`"-_8\n"`, `"-\r_8"`, `"-_8\r\n"`} {
		if err := json.Unmarshal([]byte(in), &out); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
	if err := json.Unmarshal([]byte(`"-_8"`), &out); err != nil || !bytes.Equal(out, []byte{0xfb, 0xff}) {
		t.Fatalf("%v %v", out, err)
	}
}
