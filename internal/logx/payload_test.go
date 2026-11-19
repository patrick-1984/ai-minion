package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRedactPayloadMasksSecretKeysAndTokens(t *testing.T) {
	key := "mn_" + strings.Repeat("k", 52)
	in := `{"type":"op.request","op":"pipe_send","args":{"text":"use ` + key + ` and MN-ABCD-EFGH","authorization":"Bearer abc","nested":{"api_key":"sekret","list":[{"password":"p"},"mnj1-` + strings.Repeat("Zz9", 20) + `"]}},"n":12345678901234567890}`
	out := RedactPayload([]byte(in), 16<<10)
	if !json.Valid([]byte(out)) {
		t.Fatalf("not JSON: %s", out)
	}
	for _, leak := range []string{strings.Repeat("k", 52), "ABCD-EFGH", "Bearer abc", "sekret", `"password":"p"`, strings.Repeat("Zz9", 20)} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q survived: %s", leak, out)
		}
	}
	if !strings.Contains(out, "mn_[redacted]") || !strings.Contains(out, `"n":12345678901234567890`) || !strings.Contains(out, `"op":"pipe_send"`) {
		t.Fatalf("masking changed more than it should: %s", out)
	}
}

func TestRedactPayloadAlwaysValidAndWithinTheCap(t *testing.T) {
	big := strings.Repeat("\u00e9\"\\\n ", 20000)
	b, _ := json.Marshal(map[string]string{"screen": big, "k": "mn_" + strings.Repeat("a", 52)})
	for _, max := range []int{64, 200, 1 << 10, 12 << 10} {
		out := RedactPayload(b, max)
		if len(out) > max || !json.Valid([]byte(out)) || !utf8.ValidString(out) {
			t.Fatalf("max %d: %d bytes, valid json %v", max, len(out), json.Valid([]byte(out)))
		}
		if strings.Contains(out, strings.Repeat("a", 52)) {
			t.Fatal("a key survived in the head")
		}
		if max >= 200 && !strings.Contains(out, `"truncated":`) {
			t.Fatalf("a body over the cap must say so: %.80s", out)
		}
	}
	if out := RedactPayload([]byte("not json {"), 100); out != `{"invalid_json":true,"bytes":10}` {
		t.Fatal(out)
	}
	if out := RedactPayload([]byte(`{"a":1}`), 100); out != `{"a":1}` {
		t.Fatal(out)
	}
}

// RemoteSafe passes a payload attribute whole (it is already valid JSON within
// its cap) and never cuts it in the middle, while an ordinary string is still
// cut at 2 KiB.
func TestRemoteSafeKeepsPayloadValidJSON(t *testing.T) {
	var buf bytes.Buffer
	h := RemoteSafe(slog.NewJSONHandler(&buf, nil))
	payload := RedactPayload([]byte(`{"screen":"`+strings.Repeat("x", 8000)+`"}`), RemotePayloadMax)
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "worker traffic", 0)
	rec.AddAttrs(slog.String(PayloadKey, payload), slog.String("other", strings.Repeat("y", 5000)))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	p, _ := got[PayloadKey].(string)
	if !json.Valid([]byte(p)) || !strings.Contains(p, strings.Repeat("x", 8000)) {
		t.Fatalf("payload was cut or changed: %d bytes", len(p))
	}
	if o, _ := got["other"].(string); len(o) > RemoteMaxString {
		t.Fatalf("an ordinary string was not capped: %d", len(o))
	}
	// A payload over the cap becomes a valid marker.
	buf.Reset()
	rec = slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0)
	rec.AddAttrs(slog.String(PayloadKey, `{"a":"`+strings.Repeat("z", RemotePayloadMax)+`"}`))
	_ = h.Handle(context.Background(), rec)
	_ = json.Unmarshal(buf.Bytes(), &got)
	if p, _ := got[PayloadKey].(string); !json.Valid([]byte(p)) || len(p) > RemotePayloadMax {
		t.Fatalf("oversized payload: %.60s", p)
	}
}
