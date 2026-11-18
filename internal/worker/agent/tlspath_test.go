package agent

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Finding 8 of the round-1 review: hub_tls is an optional diagnostic. A
// malformed member never fails the welcome, and "keys differ" is reported
// only when both keys exist.
func TestHubTLSIsOptionalAndComparedOnlyWithBothKeys(t *testing.T) {
	seen := transport.TLSSeen{KeyHash: sha256.Sum256([]byte("spki")), Name: "hub.example"}
	for _, raw := range []string{
		`{"mode":"own","key_sha256":"%%%not base64"}`,
		`{"mode":"own","key_sha256":12}`,
		`{"mode":"own","key_sha256":"AAAA"}`,
		`{"mode":"own"}`,
		`{"mode":7}`,
		`"own"`,
		`[1,2]`,
	} {
		body := `{"type":"` + proto.TypeWelcome + `","worker_id":"0190a0b2-0000-7000-8000-000000000001","owner":"o","name":"n","kind":"private","policy":{},"hub_tls":` + raw + `}`
		r, err := decodeReply(json.RawMessage(body))
		if err != nil {
			t.Fatalf("%s: the welcome failed: %v", raw, err)
		}
		state, line := TLSPath(seen, r.Welcome.HubTLS)
		if state != TLSPathUnknown {
			t.Errorf("%s: state %s (%s), want unknown", raw, state, line)
		}
	}
	key := seen.KeyHash
	if s, _ := TLSPath(seen, &HubTLSInfo{Mode: "own", KeyHash: key[:]}); s != TLSPathKeysMatch {
		t.Errorf("same key: %s", s)
	}
	other := sha256.Sum256([]byte("other"))
	if s, _ := TLSPath(seen, &HubTLSInfo{Mode: "own", KeyHash: other[:]}); s != TLSPathKeysDiffer {
		t.Errorf("other key: %s", s)
	}
	if s, _ := TLSPath(seen, &HubTLSInfo{Mode: "off"}); s != TLSPathPlainToHub {
		t.Errorf("off: %s", s)
	}
}
