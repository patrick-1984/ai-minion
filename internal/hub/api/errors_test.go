package api

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got with testdata/<name>.golden, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file\n got: %q\nwant: %q", name, got, string(want))
	}
}

func TestEnvelopeGolden(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"unauthenticated", unauthenticated("sign in first")},
		{"forbidden", NewError(proto.CodeForbidden, "not allowed")},
		{"not_found", NewError(proto.CodeNotFound, "not found")},
		{"invalid_argument_field", &ops.ArgumentError{Field: "limit", Reason: "must be between 0 and 200"}},
		{"invalid_argument_no_field", &ops.ArgumentError{Reason: "timestamp must be RFC 3339 UTC with milliseconds"}},
		{"rate_limited", &Error{Code: proto.CodeRateLimited, Message: "too many failed API key attempts", RetryAfter: 1500 * time.Millisecond}},
		{"unsupported_op", NewError(proto.CodeUnsupportedOp, "the operation is not available on this Hub").WithDetails(map[string]any{"op": "share_get"})},
		{"method_not_allowed", NewError(proto.CodeMethodNotAllowed, "use POST for this route")},
		{"proto_error_with_details", &proto.Error{Code: proto.CodeConflict, Message: "setting is set by environment", Details: json.RawMessage(`{"key":"session_ttl"}`)}},
		{"internal_hides_cause", fmt.Errorf("open /var/lib/minion/minion.db: %w", errBoom)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteError(w, c.err)
			golden(t, "error_"+c.name, fmt.Sprintf("%d\n%s", w.Code, w.Body.String()))
		})
	}
}

func TestEnvelopeShape(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, NewError(proto.CodeNotFound, "x"))
	var body map[string]map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	e := body["error"]
	if len(body) != 1 || len(e) != 3 || e["code"] == nil || e["message"] == nil || string(e["details"]) != "{}" {
		t.Errorf("envelope = %s", w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", w.Header())
	}
}

// Every declared code is written with the status of proto.HTTPStatus.
func TestStatusComesFromProto(t *testing.T) {
	for _, c := range proto.Codes() {
		w := httptest.NewRecorder()
		WriteError(w, NewError(c, "m"))
		if w.Code != proto.HTTPStatus(c) {
			t.Errorf("%s: status %d, want %d", c, w.Code, proto.HTTPStatus(c))
		}
		var body struct{ Error struct{ Code proto.Code } }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body.Error.Code != c {
			t.Errorf("%s: body code %q", c, body.Error.Code)
		}
	}
}

func TestHeadersOfSpecialErrors(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, unauthenticated("x"))
	if got := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	w = httptest.NewRecorder()
	WriteError(w, &Error{Code: proto.CodeRateLimited, Message: "x", RetryAfter: 1100 * time.Millisecond})
	if got := w.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2 (rounded up)", got)
	}
	w = httptest.NewRecorder()
	WriteError(w, NewError(proto.CodeForbidden, "x"))
	if w.Header().Get("Retry-After") != "" || w.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("unexpected headers %v", w.Header())
	}
}

func TestErrorOfMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code proto.Code
	}{
		{"api error", NewError(proto.CodeConflict, "c"), proto.CodeConflict},
		{"wrapped api error", fmt.Errorf("x: %w", NewError(proto.CodeBusy, "b")), proto.CodeBusy},
		{"proto error", proto.NewError(proto.CodeForbidden, "no"), proto.CodeForbidden},
		{"argument error", &ops.ArgumentError{Field: "f", Reason: "r"}, proto.CodeInvalidArgument},
		{"wrapped sentinel", fmt.Errorf("x: %w", ops.ErrInvalidArgument), proto.CodeInvalidArgument},
		{"authz not found", authz.ErrNotFound, proto.CodeNotFound},
		{"not wired fails closed", authz.ErrNotWired, proto.CodeInternal},
		{"plain error", errBoom, proto.CodeInternal},
		{"unknown code", &proto.Error{Code: "made_up", Message: "m"}, proto.CodeInternal},
		{"nil", nil, proto.CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ErrorOf(c.err).Code; got != c.code {
				t.Errorf("code = %s, want %s", got, c.code)
			}
		})
	}
	// The field of an argument error reaches the details; a cause never
	// reaches the message.
	e := ErrorOf(&ops.ArgumentError{Field: "limit", Reason: "bad"})
	if e.Details["field"] != "limit" || e.Message != "bad" {
		t.Errorf("argument error = %+v", e)
	}
	if m := ErrorOf(errBoom).Message; strings.Contains(m, "secret") || strings.Contains(m, "/var") {
		t.Errorf("internal message leaks the cause: %q", m)
	}
}

func TestErrorIs(t *testing.T) {
	var err error = NewError(proto.CodeNotFound, "x")
	if !errors.Is(err, proto.NewError(proto.CodeNotFound, "")) || errors.Is(err, proto.NewError(proto.CodeConflict, "")) {
		t.Error("errors.Is must compare codes with *proto.Error")
	}
	if !errors.Is(err, NewError(proto.CodeNotFound, "other")) {
		t.Error("errors.Is must compare codes with *Error")
	}
}

func TestWriteErrorBadDetailsStillEnvelopes(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, NewError(proto.CodeInternal, "x").WithDetails(map[string]any{"f": make(chan int)}))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"details":{}`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}
