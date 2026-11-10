package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestFormatTimeIsRFC3339UTCWithMilliseconds(t *testing.T) {
	loc := time.FixedZone("x", 2*3600)
	cases := map[string]time.Time{
		"2026-11-09T10:00:00.000Z": time.Date(2026, 11, 9, 12, 0, 0, 0, loc),
		"2026-11-09T10:00:00.123Z": time.Date(2026, 11, 9, 10, 0, 0, 123_456_789, time.UTC),
		"1970-01-01T00:00:00.007Z": time.UnixMilli(7),
	}
	for want, in := range cases {
		if got := FormatTime(in); got != want {
			t.Errorf("FormatTime(%v) = %q, want %q", in, got, want)
		}
		back, err := ParseTime(want)
		if err != nil || !back.Equal(in.Truncate(time.Millisecond)) {
			t.Errorf("ParseTime(%q) = %v, %v", want, back, err)
		}
	}
	for _, bad := range []string{"2026-11-09T10:00:00Z", "2026-11-09T12:00:00.000+02:00", "2026-11-09 10:00:00.000Z"} {
		if _, err := ParseTime(bad); err == nil {
			t.Errorf("ParseTime(%q) succeeded", bad)
		}
	}
	// The catalog's own time type writes the same format.
	b, _ := json.Marshal(ops.Timestamp(cases["2026-11-09T10:00:00.123Z"]))
	if string(b) != `"2026-11-09T10:00:00.123Z"` {
		t.Errorf("ops.Timestamp = %s", b)
	}
}

// Every time in an operation result goes through ops.Timestamp, so no result
// can be written in another layout. A raw time.Time field would encode with
// nanoseconds and a zone.
func TestNoRawTimeInCatalogTypes(t *testing.T) {
	timeType := reflect.TypeFor[time.Time]()
	var walk func(path string, ty reflect.Type, seen map[reflect.Type]bool)
	walk = func(path string, ty reflect.Type, seen map[reflect.Type]bool) {
		switch ty.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(path, ty.Elem(), seen)
		case reflect.Map:
			walk(path, ty.Elem(), seen)
		case reflect.Struct:
			if ty == timeType {
				t.Errorf("%s is a raw time.Time; use ops.Timestamp (RFC 3339 UTC with milliseconds)", path)
				return
			}
			if ty == reflect.TypeFor[ops.Timestamp]() || seen[ty] {
				return
			}
			seen[ty] = true
			for i := 0; i < ty.NumField(); i++ {
				f := ty.Field(i)
				if f.IsExported() && f.Tag.Get("json") != "-" {
					walk(path+"."+f.Name, f.Type, seen)
				}
			}
		}
	}
	for _, op := range ops.All() {
		walk(op.Name+".args", op.Args, map[reflect.Type]bool{})
		walk(op.Name+".result", op.Result, map[reflect.Type]bool{})
	}
}

func TestReadObject(t *testing.T) {
	ok := map[string]int{`{"a":1,"b":"x","unknown_field":{"z":[1]}}`: 3, ``: 0, "  \n": 0, `null`: 0, `{}`: 0}
	for body, n := range ok {
		o, err := readObject(strings.NewReader(body))
		if err != nil || len(o) != n {
			t.Errorf("readObject(%q) = %v, %v", body, o, err)
		}
	}
	bad := map[string]proto.Code{
		`{`:        proto.CodeInvalidArgument,
		`[1]`:      proto.CodeInvalidArgument,
		`"x"`:      proto.CodeInvalidArgument,
		`{} {}`:    proto.CodeInvalidArgument,
		`{"a":1}x`: proto.CodeInvalidArgument,
		`{}]`:      proto.CodeInvalidArgument,
		`{}}`:      proto.CodeInvalidArgument,
		`{}]}`:     proto.CodeInvalidArgument,
		`{"a":1})`: proto.CodeInvalidArgument,
		`{}  ]`:    proto.CodeInvalidArgument,
	}
	for body, code := range bad {
		if _, err := readObject(strings.NewReader(body)); ErrorOf(err).Code != code {
			t.Errorf("readObject(%q) error = %v, want %s", body, err, code)
		}
	}
	big := `{"a":"` + strings.Repeat("x", MaxBodyBytes) + `"}`
	if _, err := readObject(strings.NewReader(big)); ErrorOf(err).Code != proto.CodeMessageTooLarge {
		t.Errorf("oversized body error = %v", err)
	}
	if o, err := readObject(nil); err != nil || len(o) != 0 {
		t.Errorf("readObject(nil) = %v, %v", o, err)
	}
}

func TestUnknownFieldsAreIgnoredAndKnownDecoded(t *testing.T) {
	o, err := readObject(strings.NewReader(`{"message":"hi","limit":3,"future_field":{"x":1},"flag":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var a pingArgs
	if err := decodeArgs(o, &a); err != nil {
		t.Fatalf("unknown fields must be ignored (AD-4): %v", err)
	}
	if a.Message != "hi" || a.Limit != 3 || !a.Flag {
		t.Errorf("args = %+v", a)
	}
}

func TestDecodeArgsErrors(t *testing.T) {
	cases := map[string]string{
		`{"limit":"three"}`:     "limit",
		`{"message":5}`:         "message",
		`{"at":"2026-11-09"}`:   "",
		`{"tags":"not-a-list"}`: "tags",
	}
	for body, field := range cases {
		o, _ := readObject(strings.NewReader(body))
		err := decodeArgs(o, &pingArgs{})
		e := ErrorOf(err)
		if e.Code != proto.CodeInvalidArgument {
			t.Errorf("%s: code %s (%v)", body, e.Code, err)
			continue
		}
		if field != "" && e.Details["field"] != field {
			t.Errorf("%s: details %v, want field %q", body, e.Details, field)
		}
	}
}

func TestQueryValuesAreTypedByTheArguments(t *testing.T) {
	q := url.Values{
		"message": {"hello"}, "limit": {"25"}, "flag": {"true"},
		"at":   {"2026-11-09T10:00:00.123Z"},
		"tags": {"a", "b"}, "not_a_field": {"ignored"},
	}
	o := rawObject{}
	if err := o.addValues(reflect.TypeFor[pingArgs](), q); err != nil {
		t.Fatal(err)
	}
	var a pingArgs
	if err := decodeArgs(o, &a); err != nil {
		t.Fatal(err)
	}
	if a.Message != "hello" || a.Limit != 25 || !a.Flag || len(a.Tags) != 2 || a.At == nil ||
		FormatTime(a.At.Time()) != "2026-11-09T10:00:00.123Z" {
		t.Errorf("args = %+v", a)
	}
}

func TestQueryNestedTargetMembers(t *testing.T) {
	o := rawObject{}
	err := o.addValues(reflect.TypeFor[ops.SessionSnapshotArgs](), url.Values{
		"target.worker_id": {"w1"}, "target.session_name": {"minion-1"}, "target.pane": {"0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var a ops.SessionSnapshotArgs
	if err := decodeArgs(o, &a); err != nil {
		t.Fatal(err)
	}
	if a.Target.WorkerID != "w1" || a.Target.SessionName != "minion-1" || a.Target.Pane != "0.1" {
		t.Errorf("target = %+v", a.Target)
	}
}

func TestQueryErrors(t *testing.T) {
	cases := []struct {
		q     url.Values
		field string
	}{
		{url.Values{"limit": {"x"}}, "limit"},
		{url.Values{"limit": {"1", "2"}}, "limit"},
		{url.Values{"flag": {"maybe"}}, "flag"},
	}
	for _, c := range cases {
		err := rawObject{}.addValues(reflect.TypeFor[pingArgs](), c.q)
		var ae *Error
		if !errors.As(err, &ae) || ae.Code != proto.CodeInvalidArgument || ae.Details["field"] != c.field {
			t.Errorf("%v: err = %v", c.q, err)
		}
	}
	// Structures cannot travel in a query string.
	type withObj struct {
		Obj struct{ A int } `json:"obj"`
	}
	if err := (rawObject{}).addValues(reflect.TypeFor[withObj](), url.Values{"obj": {"x"}}); err == nil {
		t.Error("an object-typed argument was accepted from a query string")
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	if err := WriteJSON(w, 200, pingResult{Echo: "x", At: ops.Timestamp(time.UnixMilli(1_700_000_000_123))}); err != nil {
		t.Fatal(err)
	}
	if w.Body.String() != `{"echo":"x","at":"2023-11-14T22:13:20.123Z"}`+"\n" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%q %v", w.Body, w.Header())
	}
	w = httptest.NewRecorder()
	if err := WriteJSON(w, 200, make(chan int)); err == nil || w.Code != 500 || !strings.Contains(w.Body.String(), `"internal"`) {
		t.Errorf("unencodable value: %v %d %s", err, w.Code, w.Body)
	}
}

// Only exact member names reach the decoder, at every depth.
func TestOnlyExactMemberNamesAreDecoded(t *testing.T) {
	type inner struct {
		WorkerID string `json:"worker_id"`
	}
	type outer struct {
		Message string  `json:"message"`
		Target  *inner  `json:"target"`
		List    []inner `json:"list"`
		At      *ops.Timestamp
	}
	body := `{"MESSAGE":"loud","message":"quiet","target":{"WORKER_ID":"x","wor\u212Aer_id":"y","worker_id":"exact"},` +
		`"list":[{"Worker_ID":"x"},{"worker_id":"ok"}],"At":"2026-11-09T10:00:00.000Z"}`
	o, err := readObject(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var got outer
	if err := decodeArgs(o, &got); err != nil {
		t.Fatal(err)
	}
	if got.Message != "quiet" || got.Target == nil || got.Target.WorkerID != "exact" ||
		len(got.List) != 2 || got.List[0].WorkerID != "" || got.List[1].WorkerID != "ok" || got.At == nil {
		t.Errorf("decoded %+v target=%+v", got, got.Target)
	}
}
