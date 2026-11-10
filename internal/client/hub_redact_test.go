package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// uescape spells every character of s as a JSON \u escape, which a search of
// the raw bytes for the text does not find.
func uescape(s string) string {
	var b strings.Builder
	for _, c := range s {
		fmt.Fprintf(&b, `\u%04x`, c)
	}
	return b.String()
}

func detailsOf(t *testing.T, err error) (*proto.Error, string) {
	t.Helper()
	var pe *proto.Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected a protocol error: %v", err)
	}
	return pe, string(pe.Details)
}

// The details of an upstream error are inspected after decoding, so the key
// does not survive in them however JSON spells it, and what is legitimate keeps
// its type and its exact numbers.
func TestHubErrorDetailsAreRedactedAfterDecoding(t *testing.T) {
	var key string
	var body func() string
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		key = f.key
		jsonReply(w, 400, body())
	})
	b := f.hub(t)
	k := f.key
	cases := []struct {
		name, details string
		dropped       bool   // the whole details are dropped
		keep          string // a fragment that must remain (legitimate data)
	}{
		{"escaped value", `{"echo":"` + uescape(k) + `"}`, false, ``},
		{"escaped bearer", `{"echo":"` + uescape("Bearer "+k) + `"}`, false, ``},
		{"escaped key shape", `{"echo":"` + uescape("mn_abcdefghijklmnopqrstuvwxyz") + `"}`, false, ``},
		{"nested object and array", `{"a":{"b":[1,{"c":"x ` + uescape(k) + ` y"}]}}`, false, ``},
		{"escaped member name", `{"` + uescape(k) + `":1}`, true, ``},
		{"mixed escapes", `{"echo":"` + k[:6] + uescape(k[6:]) + `"}`, false, ``},
		{"legitimate details", `{"field":"name","limit":9007199254740993,"big":123456789012345678901234567890,"exp":1e400,"ok":true,"none":null,"list":["a",2]}`, false,
			`"limit":9007199254740993`},
		{"too deep", strings.Repeat(`[`, maxDetailDepth+3) + strings.Repeat(`]`, maxDetailDepth+3), true, ``},
	}
	for _, c := range cases {
		body = func() string {
			return `{"error":{"code":"invalid_argument","message":"bad ` + uescape(k) + `","details":` + c.details + `}}`
		}
		_, err := b.Call(context.Background(), ops.OpSessionsList, nil)
		pe, d := detailsOf(t, err)
		if pe.Code != proto.CodeInvalidArgument || strings.Contains(pe.Message, key) || !strings.Contains(pe.Message, "[redacted]") {
			t.Errorf("%s: code %q, message %q", c.name, pe.Code, pe.Message)
		}
		decoded := mustDecode(t, d)
		// Whatever the encoding, no decoded string of the result may hold it.
		if flat := flatten(decoded); strings.Contains(flat, key) || strings.Contains(flat, "mn_abcdefghijklmnop") || strings.Contains(strings.ToLower(flat), "bearer "+key) {
			t.Errorf("%s: a secret survives in the details: %s", c.name, d)
		}
		if strings.Contains(d, key) || strings.Contains(err.Error(), key) {
			t.Errorf("%s: the key is in the result or the error text", c.name)
		}
		if c.dropped && d != "" {
			t.Errorf("%s: expected the details to be dropped, got %s", c.name, d)
		}
		if !c.dropped && d == "" {
			t.Errorf("%s: the details were dropped instead of redacted", c.name)
		}
		if c.keep != "" && !strings.Contains(d, c.keep) {
			t.Errorf("%s: legitimate data was changed: %s", c.name, d)
		}
	}
}

func flatten(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, e := range x {
			parts = append(parts, flatten(e))
		}
		return strings.Join(parts, " ")
	case map[string]any:
		var parts []string
		for k, e := range x {
			parts = append(parts, k, flatten(e))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// A stream that fails after it started carries its error in the last line; its
// details get the same treatment, and a refusal before anything was produced
// (the other path) too.
func TestHubStreamErrorDetailsAreRedactedAfterDecoding(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		fl := ndjson(w)
		_, _ = fmt.Fprintln(w, `{"turn":1}`)
		fl.Flush()
		_, _ = fmt.Fprintf(w, `{"error":{"code":"conflict","message":"x","details":{"echo":["%s"]}}}`+"\n", uescape(f.key))
		fl.Flush()
	})
	var got int
	_, err := f.hub(t).Stream(context.Background(), ops.OpPipeWait, map[string]any{}, func(json.RawMessage) error { got++; return nil })
	pe, d := detailsOf(t, err)
	if got != 1 || pe.Code != proto.CodeConflict || strings.Contains(d, f.key) || strings.Contains(flatten(mustDecode(t, d)), f.key) {
		t.Errorf("elements %d, code %q, details %s", got, pe.Code, d)
	}
}

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if s == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber() // 1e400 does not fit a float64
	if dec.Decode(&v) != nil {
		t.Fatalf("not JSON: %s", s)
	}
	return v
}

// Transport errors and the origin label that messages show never hold a key
// that sits in the Hub's host name.
func TestHubURLThatLooksLikeAKeyIsRefusedWithoutBeingRepeated(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	for name, url := range map[string]string{
		"host":            "https://" + f.key + ".invalid",
		"upper host":      "https://" + strings.ToUpper(f.key) + ".invalid",
		"host with port":  "https://" + f.key + ".invalid:8443",
		"path":            "https://hub.example/" + f.key,
		"encoded host":    "https://%6d%6e_" + f.key[3:] + ".invalid",
		"encoded path":    "https://hub.example/%6dn_" + f.key[3:],
		"key shaped only": "https://mn_abcdefghijklmnopqrstuvwxyz.invalid",
	} {
		cfg := f.cfg()
		cfg.URL = url
		b, err := NewHub(cfg)
		if b != nil {
			b.Close()
		}
		if !errors.Is(err, ErrHubURL) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if strings.Contains(err.Error(), f.key) || strings.Contains(strings.ToLower(err.Error()), "mn_abcdefghijklmnop") {
			t.Errorf("%s: the refusal repeats the URL: %v", name, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Error("a refused URL was contacted")
	}
}

// The Hub's REST API takes only an id in worker_id, so a default Worker that is
// not a UUIDv7 is refused before anything is sent (no wrong target, no first
// Worker): the contract is UUID only.
func TestHubDefaultWorkerMustBeAWorkerID(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	for _, w := range []string{"named-worker", "w-1", "lab:alpha", "01930000-0000-7000-8000-00000000000G", "01930000-0000-4000-8000-000000000001",
		"01930000-0000-7000-8000-00000000000A", "{01930000-0000-7000-8000-000000000001}"} {
		cfg := f.cfg()
		cfg.Worker = w
		b, err := NewHub(cfg)
		if b != nil {
			b.Close()
		}
		if !errors.Is(err, ErrHubWorker) {
			t.Errorf("default Worker %q: %v", w, err)
		}
	}
	cfg := f.cfg()
	cfg.Worker = "  01930000-0000-7000-8000-000000000001 "
	b, err := NewHub(cfg)
	if err != nil {
		t.Fatalf("a Worker id with blanks around it: %v", err)
	}
	b.Close()
	if f.hits.Load() != 0 {
		t.Error("a request was made")
	}
}

// Details that are not exactly one JSON value are dropped, never passed on.
func TestScrubJSONDropsWhatIsNotOneValue(t *testing.T) {
	const key = "mn_abcdefghijklmnopqrstuvwxyzabcdef"
	for name, in := range map[string]string{
		"two values": `{"a":1} {"echo":"` + key + `"}`,
		"garbage":    `{"a":`,
		"empty":      ``,
	} {
		if got := scrubJSON(json.RawMessage(in), key); got != nil {
			t.Errorf("%s: %s", name, got)
		}
	}
	if got := string(scrubJSON(json.RawMessage(` {"b":2,"a":"<&>"} `), key)); got != `{"a":"<&>","b":2}` {
		t.Errorf("a clean value changed: %s", got)
	}
}
