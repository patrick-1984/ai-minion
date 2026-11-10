package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

func echoUser() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, ok := FromContext(r.Context()); ok {
			_, _ = w.Write([]byte("user=" + s.UserID))
			return
		}
		_, _ = w.Write([]byte("anonymous"))
	})
}

func do(t *testing.T, h http.Handler, method, token, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/x", nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: token})
	}
	if csrf != "" {
		req.Header.Set(CSRFHeader, csrf)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) proto.Code {
	t.Helper()
	var body struct {
		Error struct {
			Code    proto.Code     `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", rec.Body, err)
	}
	if body.Error.Message == "" || body.Error.Details == nil {
		t.Errorf("envelope %q lacks message or details", rec.Body)
	}
	if rec.Code != proto.HTTPStatus(body.Error.Code) {
		t.Errorf("status %d does not match code %s", rec.Code, body.Error.Code)
	}
	return body.Error.Code
}

func TestCSRFIsRequiredForNonGetWithASessionCookie(t *testing.T) {
	m, _, _ := newTestManager(t)
	mw := &Middleware{Manager: m}
	h := mw.Wrap(echoUser())
	is, _ := m.Create(context.Background(), alice, time.Hour)
	other, _ := m.Create(context.Background(), alice, time.Hour)

	// GET needs no token.
	if rec := do(t, h, http.MethodGet, is.Token, ""); rec.Code != 200 {
		t.Errorf("GET without CSRF = %d", rec.Code)
	}
	// Every other method without a valid token is forbidden, and succeeds with it.
	for _, method := range []string{http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for name, csrf := range map[string]string{"missing": "", "wrong": "nope", "other session": other.CSRFToken, "cookie value": is.Token} {
			rec := do(t, h, method, is.Token, csrf)
			if rec.Code != http.StatusForbidden || errCode(t, rec) != proto.CodeForbidden {
				t.Errorf("%s with %s CSRF = %d %s", method, name, rec.Code, rec.Body)
			}
		}
		rec := do(t, h, method, is.Token, is.CSRFToken)
		if rec.Code != 200 {
			t.Errorf("%s with valid CSRF = %d %q", method, rec.Code, rec.Body)
		}
		if method != http.MethodHead && rec.Body.String() != "user="+alice.ID {
			t.Errorf("%s with valid CSRF body = %q", method, rec.Body)
		}
	}
}

func TestCSRFAppliesToStaleOrMalformedCookiesToo(t *testing.T) {
	m, _, _ := newTestManager(t)
	h := (&Middleware{Manager: m}).Wrap(echoUser())
	for _, token := range []string{"garbage", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if rec := do(t, h, http.MethodPost, token, ""); rec.Code != http.StatusForbidden {
			t.Errorf("POST with stale cookie %q = %d", token, rec.Code)
		}
	}
}

func TestRequestsWithoutACookieAreAnonymousAndNotCSRFChecked(t *testing.T) {
	m, _, _ := newTestManager(t)
	h := (&Middleware{Manager: m}).Wrap(echoUser())
	rec := do(t, h, http.MethodPost, "", "")
	if rec.Code != 200 || rec.Body.String() != "anonymous" {
		t.Errorf("anonymous POST = %d %q (bearer-token callers have no cookie)", rec.Code, rec.Body)
	}
}

func TestExpiredSessionIsAnonymous(t *testing.T) {
	m, _, clk := newTestManager(t)
	h := (&Middleware{Manager: m}).Wrap(echoUser())
	is, _ := m.Create(context.Background(), alice, time.Minute)
	clk.Add(time.Hour)
	rec := do(t, h, http.MethodGet, is.Token, "")
	if rec.Body.String() != "anonymous" {
		t.Errorf("expired session = %q", rec.Body)
	}
}

func TestRequireAnswersUnauthenticated(t *testing.T) {
	m, _, _ := newTestManager(t)
	mw := &Middleware{Manager: m}
	h := mw.Protect(echoUser())
	rec := do(t, h, http.MethodGet, "", "")
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != proto.CodeUnauthenticated {
		t.Errorf("no session = %d %s", rec.Code, rec.Body)
	}
	is, _ := m.Create(context.Background(), alice, time.Hour)
	if rec := do(t, h, http.MethodGet, is.Token, ""); rec.Code != 200 {
		t.Errorf("valid session = %d", rec.Code)
	}
}

func TestStoreFailureIsInternalNotAnonymous(t *testing.T) {
	m, ms, _ := newTestManager(t)
	h := (&Middleware{Manager: m}).Wrap(echoUser())
	is, _ := m.Create(context.Background(), alice, time.Hour)
	ms.failRead = errors.New("db down")
	rec := do(t, h, http.MethodGet, is.Token, "")
	if rec.Code != http.StatusInternalServerError || errCode(t, rec) != proto.CodeInternal {
		t.Errorf("store failure = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "db down") {
		t.Error("the store error leaks into the response")
	}
}
