package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

type countingBackend struct {
	calls atomic.Int32
	err   error
}

func (c *countingBackend) Call(context.Context, string, any) (json.RawMessage, error) {
	c.calls.Add(1)
	return json.RawMessage(`{"local":true}`), c.err
}

func (c *countingBackend) Stream(ctx context.Context, op string, args any, _ func(json.RawMessage) error) (json.RawMessage, error) {
	return c.Call(ctx, op, args)
}

// The matrix of the story: which backend serves a command, for every
// combination of "Hub configured", "local Worker present" and "Hub usable".
func TestSelectMatrix(t *testing.T) {
	hub := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{"hub":true}`) })
	good := hub.cfg()
	dir := t.TempDir()
	badKey := writeFile(t, dir, "bad", []byte("not a key"))

	type want struct {
		kind   Kind
		err    error
		answer string // "hub", "local", or "" when there is an error
	}
	for _, tc := range []struct {
		name  string
		cfg   HubConfig
		local bool // a local Worker is present
		want  want
	}{
		{"hub configured, no local", good, false, want{KindHub, nil, "hub"}},
		{"hub configured, local present: the Hub wins", good, true, want{KindHub, nil, "hub"}},
		{"no hub, local present", HubConfig{}, true, want{KindLocal, nil, "local"}},
		{"no hub, no local socket", HubConfig{}, false, want{KindLocal, ErrNoBackend, ""}},
		{"hub unusable key file, local present: no fallback", HubConfig{URL: good.URL, APIKeyFile: badKey, CAFile: good.CAFile}, true, want{KindHub, ErrAPIKeyFile, ""}},
		{"hub missing key file, local present: no fallback", HubConfig{URL: good.URL, CAFile: good.CAFile}, true, want{KindHub, ErrAPIKeyMissing, ""}},
		{"key file without a URL, local present: not silently local", HubConfig{APIKeyFile: good.APIKeyFile}, true, want{KindHub, ErrHubURL, ""}},
		{"bad URL, local present: no fallback", HubConfig{URL: "ftp://x", APIKeyFile: good.APIKeyFile}, true, want{KindHub, ErrHubURL, ""}},
		{"unreadable CA, local present: no fallback", HubConfig{URL: good.URL, APIKeyFile: good.APIKeyFile, CAFile: dir + "/none"}, true, want{KindHub, ErrHubCA, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := &countingBackend{}
			var lb Backend
			if tc.local {
				lb = local
			} else {
				lb = &countingBackend{err: ErrWorkerNotRunning} // nothing serves the socket
			}
			b, kind, err := Select(tc.cfg, lb)
			if kind != tc.want.kind {
				t.Errorf("kind = %s, want %s", kind, tc.want.kind)
			}
			if tc.want.err == nil && err != nil {
				t.Fatalf("Select: %v", err)
			}
			if tc.want.err != nil {
				// A Worker that is not running shows when the call is made.
				if err == nil {
					_, err = b.Call(context.Background(), ops.OpSessionsList, nil)
				}
				if !errors.Is(err, tc.want.err) {
					t.Fatalf("error = %v, want %v", err, tc.want.err)
				}
				if tc.want.kind == KindHub && local.calls.Load() != 0 {
					t.Error("the local Worker was used after the Hub failed")
				}
				return
			}
			out, err := b.Call(context.Background(), ops.OpSessionsList, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(out); (tc.want.answer == "hub") != strings.Contains(got, "hub") {
				t.Errorf("answered by the wrong backend: %s", got)
			}
			if tc.want.answer == "hub" && local.calls.Load() != 0 {
				t.Error("the local Worker was called although a Hub is configured")
			}
		})
	}
}

func TestSelectedHubFailureNeverRetargetsTheEffect(t *testing.T) {
	// The Hub refuses; the local backend, which is present, must not be tried
	// for the same write.
	hub := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 403, envelopeJSON("forbidden", "no"))
	})
	local := &countingBackend{}
	b, _, err := Select(hub.cfg(), local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), ops.OpSendText, map[string]any{"request_id": "r"}); err == nil {
		t.Fatal("expected a refusal")
	}
	if local.calls.Load() != 0 || hub.hits.Load() != 1 {
		t.Errorf("local calls %d, hub hits %d", local.calls.Load(), hub.hits.Load())
	}
}

func TestNoHubHintKeepsTheWorkerErrorAndAddsTheConfigurationHint(t *testing.T) {
	b, _, err := Select(HubConfig{}, &countingBackend{err: ErrWorkerNotRunning})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Call(context.Background(), ops.OpSessionsList, nil)
	if !errors.Is(err, ErrWorkerNotRunning) || !errors.Is(err, ErrNoBackend) {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{"MINION_HUB_URL", "MINION_API_KEY_FILE", "no Hub is configured"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// The hint wraps once, and other errors pass through unchanged.
	again := NoHubHint(b)
	if _, err2 := again.Call(context.Background(), ops.OpSessionsList, nil); strings.Count(err2.Error(), "no Hub is configured") != 1 {
		t.Errorf("hinted twice: %v", err2)
	}
	other := errors.New("boom")
	if _, err := NoHubHint(&countingBackend{err: other}).Call(context.Background(), "x", nil); err != other {
		t.Errorf("other error changed: %v", err)
	}
}

func TestSelectedLocalBackendIsNotConfigured(t *testing.T) {
	// Without any backend and no Hub: a configuration error, never an invented server.
	_, kind, err := Select(HubConfig{}, nil)
	if kind != KindLocal || !errors.Is(err, ErrNoBackend) {
		t.Errorf("kind %s, error %v", kind, err)
	}
	// A key alone never selects the local backend.
	key := writeFile(t, t.TempDir(), "k", []byte(identity.NewAPIKey().String()))
	if _, _, err := Select(HubConfig{APIKeyFile: key}, &countingBackend{}); !errors.Is(err, ErrHubURL) || !strings.Contains(err.Error(), "MINION_HUB_URL") {
		t.Errorf("key without URL: %v", err)
	}
}
