package filexfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// RFC 8785 (JSON Canonicalization Scheme) for the subset files.v1 uses:
// null, booleans, strings, integers of magnitude below 2^53, arrays and
// objects. Floating point numbers are refused; integers within the subset are
// serialized as ECMAScript does (plain decimal digits), so the output is the
// JCS output for every value of the subset.

// maxSafeInteger is 2^53 - 1, the largest integer JCS serializes exactly.
const maxSafeInteger = 1<<53 - 1

var errJCSUnsupported = errors.New("jcs: value outside the supported subset")

// Canonicalize returns the RFC 8785 serialization of v. v may be nil, bool,
// string, int, int64, json.Number (an integer), []any or map[string]any,
// nested.
func Canonicalize(v any) ([]byte, error) {
	return appendJCS(nil, v)
}

// CanonicalizeJSON parses JSON text and returns its RFC 8785 serialization.
// Duplicate object keys and numbers outside the subset are refused.
func CanonicalizeJSON(b []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := decodeStrict(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err == nil {
		return nil, errors.New("jcs: trailing data after the JSON value")
	}
	return Canonicalize(v)
}

// decodeStrict decodes one JSON value token by token, refusing duplicate keys.
func decodeStrict(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		switch t {
		case '[':
			arr := []any{}
			for d.More() {
				v, err := decodeStrict(d)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		case '{':
			obj := map[string]any{}
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				if _, dup := obj[k]; dup {
					return nil, errors.New("jcs: duplicate object key")
				}
				v, err := decodeStrict(d)
				if err != nil {
					return nil, err
				}
				obj[k] = v
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		}
		return nil, errors.New("jcs: unexpected delimiter")
	default:
		return t, nil
	}
}

func appendJCS(dst []byte, v any) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, v), nil
	case string:
		return appendJCSString(dst, v)
	case int:
		return appendJCSInt(dst, int64(v))
	case int64:
		return appendJCSInt(dst, v)
	case json.Number:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil || !plainInteger(string(v)) {
			return nil, fmt.Errorf("%w: number %q", errJCSUnsupported, string(v))
		}
		return appendJCSInt(dst, n)
	case []any:
		dst = append(dst, '[')
		for i, e := range v {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendJCS(dst, e); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendJCSString(dst, k); err != nil {
				return nil, err
			}
			dst = append(dst, ':')
			if dst, err = appendJCS(dst, v[k]); err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	}
	return nil, fmt.Errorf("%w: %T", errJCSUnsupported, v)
}

// plainInteger reports whether s is an integer literal without fraction or
// exponent.
func plainInteger(s string) bool {
	if s != "" && s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func appendJCSInt(dst []byte, n int64) ([]byte, error) {
	if n > maxSafeInteger || n < -maxSafeInteger {
		return nil, fmt.Errorf("%w: integer beyond 2^53", errJCSUnsupported)
	}
	return strconv.AppendInt(dst, n, 10), nil
}

// appendJCSString writes s as a JSON string the way ECMAScript's
// JSON.stringify does (RFC 8785 section 3.2.2.2): '"' and '\' escaped, the
// short escapes \b \t \n \f \r, other control characters as \u00xx with
// lower-case hex, everything else literally. s must be valid UTF-8.
func appendJCSString(dst []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("%w: invalid UTF-8", errJCSUnsupported)
	}
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"'), nil
}

// compareUTF16 orders object keys by their UTF-16 code units (RFC 8785
// section 3.2.3).
func compareUTF16(a, b string) int {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			if ua[i] < ub[i] {
				return -1
			}
			return 1
		}
	}
	return len(ua) - len(ub)
}
