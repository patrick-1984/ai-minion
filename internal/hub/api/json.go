package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// MaxBodyBytes caps a request body: the same 1 MiB as a wire frame (AD-3).
const MaxBodyBytes = 1 << 20

// TimeLayout is the only time format of the JSON API (AD-18): RFC 3339 in UTC
// with exactly three fractional digits. ops.Timestamp encodes the same way.
const TimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders t as RFC 3339 UTC with milliseconds.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses the format of [FormatTime] and nothing else.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeLayout, s) }

// rawObject is a JSON object whose members stay encoded until the arguments
// type decodes them. The body, the query string and the path parameters of a
// request all become one rawObject, so one decoder produces the arguments.
type rawObject map[string]json.RawMessage

// readObject reads a request body into a rawObject. An empty body is the
// empty object, which suits operations without arguments; JSON null too. Any
// member is kept: unknown fields are ignored later (AD-4, additive rule).
func readObject(body io.Reader) (rawObject, error) {
	if body == nil {
		return rawObject{}, nil
	}
	b, err := io.ReadAll(io.LimitReader(body, MaxBodyBytes+1))
	if err != nil {
		return nil, invalidArg("", "the request body could not be read")
	}
	if len(b) > MaxBodyBytes {
		return nil, NewError(proto.CodeMessageTooLarge, fmt.Sprintf("the request body exceeds %d bytes", MaxBodyBytes))
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return rawObject{}, nil
	}
	var obj rawObject
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&obj); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			return nil, invalidArg("", "the request body must be a JSON object")
		}
		return nil, invalidArg("", "the request body is not valid JSON")
	}
	// More would accept a stray closing delimiter ("{}]"); only the end of
	// the input is the end of the document.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalidArg("", "the request body has data after the JSON object")
	}
	if obj == nil { // JSON null
		obj = rawObject{}
	}
	return obj, nil
}

// workerID returns the worker_id member as a string, or "" when it is absent
// or null. A member of another type is invalid_argument.
func (o rawObject) workerID() (string, error) {
	raw, ok := o["worker_id"]
	if !ok {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", invalidArg("worker_id", "has the wrong type, expected a string")
	}
	return s, nil
}

// set stores val at the dotted path (for example "target.worker_id"),
// creating or merging nested objects.
func (o rawObject) set(path []string, val json.RawMessage) error {
	if len(path) == 1 {
		o[path[0]] = val
		return nil
	}
	child := rawObject{}
	if cur, ok := o[path[0]]; ok {
		if err := json.Unmarshal(cur, &child); err != nil || child == nil {
			// The member exists with another shape: the caller's value wins.
			child = rawObject{}
		}
	}
	if err := child.set(path[1:], val); err != nil {
		return err
	}
	b, err := json.Marshal(child)
	if err != nil {
		return err
	}
	o[path[0]] = b
	return nil
}

// addValues adds query or path values to o. A name is a JSON member name, or a
// dotted path into a nested object. Values are typed by the argument type t:
// strings stay strings, numbers and booleans are parsed, repeated names make
// an array. A name that matches no field is ignored (additive rule).
func (o rawObject) addValues(t reflect.Type, vals url.Values) error {
	for name, vs := range vals {
		path := strings.Split(name, ".")
		ft, ok := fieldTypeAt(t, path)
		if !ok || len(vs) == 0 {
			continue
		}
		raw, err := coerce(name, ft, vs)
		if err != nil {
			return err
		}
		if err := o.set(path, raw); err != nil {
			return invalidArg(name, "cannot be combined with the other arguments")
		}
	}
	return nil
}

// decodeArgs decodes o into dst (a pointer to the arguments struct). Unknown
// members are ignored. Decoding errors become invalid_argument.
func decodeArgs(o rawObject, dst any) error {
	b, err := json.Marshal(exactMembers(reflect.TypeOf(dst), o))
	if err != nil {
		return invalidArg("", "the arguments could not be encoded")
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return decodeError(err)
	}
	return nil
}

// exactMembers returns the members of v that name a member of t exactly. The
// standard decoder also matches member names case-insensitively, with Unicode
// folding (the Kelvin sign matches "k"), so a body member such as
// "wor\u212Aer_id" would otherwise overwrite the exact "worker_id" that a
// path parameter filled in, and the call would act on another resource than
// its URL names. Only exact names reach the decoder; any other member is an
// unknown field and is ignored (additive rule). It applies to nested objects
// and to the elements of arrays alike. Types that decode themselves are left
// alone.
func exactMembers(t reflect.Type, o rawObject) rawObject {
	t = derefType(t)
	if t.Kind() != reflect.Struct || decodesItself(t) {
		return o
	}
	out := make(rawObject, len(o))
	for name, v := range o {
		if ft, ok := memberType(t, name); ok {
			out[name] = exactRaw(ft, v)
		}
	}
	return out
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func decodesItself(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(unmarshalerType) || t.Implements(unmarshalerType)
}

// exactRaw applies exactMembers to the JSON value raw of type t. A value of
// another shape than t is returned as it is: the decoder reports it.
func exactRaw(t reflect.Type, raw json.RawMessage) json.RawMessage {
	t = derefType(t)
	switch {
	case decodesItself(t):
		return raw
	case t.Kind() == reflect.Struct:
		var in rawObject
		if err := json.Unmarshal(raw, &in); err != nil || in == nil {
			return raw
		}
		if b, err := json.Marshal(exactMembers(t, in)); err == nil {
			return b
		}
	case t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8, t.Kind() == reflect.Array:
		var in []json.RawMessage
		if err := json.Unmarshal(raw, &in); err != nil {
			return raw
		}
		for i, v := range in {
			in[i] = exactRaw(t.Elem(), v)
		}
		if b, err := json.Marshal(in); err == nil {
			return b
		}
	}
	return raw
}

func decodeError(err error) error {
	var ute *json.UnmarshalTypeError
	switch {
	case errors.Is(err, ops.ErrInvalidArgument):
		return err
	case errors.As(err, &ute):
		field := ute.Field
		return invalidArg(field, "has the wrong type, expected "+jsonKind(ute.Type))
	default:
		return invalidArg("", "the arguments could not be decoded")
	}
}

func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	default:
		return "an object"
	}
}

// fieldTypeAt returns the Go type of the member at the dotted JSON path of t.
func fieldTypeAt(t reflect.Type, path []string) (reflect.Type, bool) {
	for _, name := range path {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return nil, false
		}
		ft, ok := memberType(t, name)
		if !ok {
			return nil, false
		}
		t = ft
	}
	return t, true
}

// memberType finds the JSON member name in struct type t, flattening embedded
// structs without a tag as encoding/json does.
func memberType(t reflect.Type, name string) (reflect.Type, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jname, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && jname == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if mt, ok := memberType(ft, name); ok {
					return mt, true
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if jname == "" {
			jname = f.Name
		}
		if jname == name {
			return f.Type, true
		}
	}
	return nil, false
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// coerce turns the string values of a query parameter into JSON for type t.
func coerce(name string, t reflect.Type, vs []string) (json.RawMessage, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		items := make([]json.RawMessage, len(vs))
		for i, v := range vs {
			raw, err := coerceScalar(name, t.Elem(), v)
			if err != nil {
				return nil, err
			}
			items[i] = raw
		}
		return json.Marshal(items)
	}
	if len(vs) > 1 {
		return nil, invalidArg(name, "must be given once")
	}
	return coerceScalar(name, t, vs[0])
}

func coerceScalar(name string, t reflect.Type, v string) (json.RawMessage, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) || t.Implements(unmarshalerType) {
		// ops.Timestamp and friends decode themselves from a JSON string.
		return json.Marshal(v)
	}
	switch t.Kind() {
	case reflect.String:
		return json.Marshal(v)
	case reflect.Bool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, invalidArg(name, "must be true or false")
		}
		return json.Marshal(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, invalidArg(name, "must be an integer")
		}
		return json.Marshal(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, invalidArg(name, "must be a non-negative integer")
		}
		return json.Marshal(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, invalidArg(name, "must be a number")
		}
		return json.Marshal(f)
	default:
		return nil, invalidArg(name, "cannot be given in the query string; use POST with a JSON body")
	}
}

// WriteJSON writes v as a JSON response with the given status. API responses
// are never cached or sniffed. An encoding failure is answered as internal
// before anything is written.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		WriteError(w, internalError())
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
	return nil
}
