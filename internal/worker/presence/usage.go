package presence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"unicode/utf8"
)

var metricRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,63}$`)

// CheckUsage validates raw against the whole minion.usage-meters/1 schema
// (docs/schemas/usage-meters.schema.json): the size bound, one JSON object, no
// data after it, no unknown field at any level, every required field present,
// every type, range, enum and length. The publisher checks before it writes and
// the reader checks before it returns a document, so a Hub bug or a planted
// file cannot publish more than the schema allows. TestUsageValidatorMatches
// Schema keeps this function and the schema file in step.
func CheckUsage(raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxUsageSize {
		return fmt.Errorf("a usage document must be 1 to %d bytes", MaxUsageSize)
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return fmt.Errorf("the usage document is ambiguous: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("the usage document is not JSON: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("the usage document has data after the JSON value")
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return errors.New("the usage document is not a JSON object")
	}
	if err := (objSpec{
		required: []string{"schema", "generated_at", "slots"},
		fields: map[string]func(any) error{
			"schema":       constString(UsageSchema),
			"generated_at": intRange(0, math.MaxInt64),
			"slots":        arrayOf(4, slotCheck),
		},
	}).check(m); err != nil {
		return fmt.Errorf("usage document: %w", err)
	}
	return nil
}

// rejectDuplicateKeys fails when any JSON object in b repeats a member name.
// The map decoders keep the last value, so the validated value would differ
// from what a reader of the original bytes sees. Malformed JSON is left to the
// decoder that follows.
func rejectDuplicateKeys(b []byte) error {
	type frame struct {
		obj     bool
		wantKey bool
		keys    map[string]bool
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var stack []*frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].obj {
			stack[n-1].wantKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		if n := len(stack); n > 0 && stack[n-1].wantKey {
			if k, ok := tok.(string); ok {
				if stack[n-1].keys[k] {
					return fmt.Errorf("duplicate member %q", k)
				}
				stack[n-1].keys[k] = true
				stack[n-1].wantKey = false
				continue
			}
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &frame{obj: true, wantKey: true, keys: map[string]bool{}})
			case '[':
				stack = append(stack, &frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		}
		valueDone()
	}
}

func checkUsage(raw []byte) error { return CheckUsage(raw) }

type objSpec struct {
	required []string
	fields   map[string]func(any) error
}

func (o objSpec) check(m map[string]any) error {
	for k, v := range m {
		f, ok := o.fields[k]
		if !ok {
			return fmt.Errorf("unknown field %q", k)
		}
		if err := f(v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	for _, k := range o.required {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("missing field %q", k)
		}
	}
	return nil
}

func objectOf(s objSpec) func(any) error {
	return func(v any) error {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("must be an object")
		}
		return s.check(m)
	}
}

func arrayOf(max int, item func(any) error) func(any) error {
	return func(v any) error {
		a, ok := v.([]any)
		if !ok {
			return errors.New("must be an array")
		}
		if len(a) > max {
			return fmt.Errorf("at most %d items", max)
		}
		for i, e := range a {
			if err := item(e); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
		return nil
	}
}

func constString(want string) func(any) error {
	return func(v any) error {
		if s, ok := v.(string); !ok || s != want {
			return fmt.Errorf("must be %q", want)
		}
		return nil
	}
}

func enumOf(vals ...string) func(any) error {
	return func(v any) error {
		s, ok := v.(string)
		if ok {
			for _, w := range vals {
				if s == w {
					return nil
				}
			}
		}
		return fmt.Errorf("must be one of %v", vals)
	}
}

func stringLen(min, max int) func(any) error {
	return func(v any) error {
		s, ok := v.(string)
		if !ok {
			return errors.New("must be a string")
		}
		if n := utf8.RuneCountInString(s); n < min || n > max {
			return fmt.Errorf("length must be %d to %d", min, max)
		}
		return nil
	}
}

func metricName(v any) error {
	if s, ok := v.(string); !ok || !metricRE.MatchString(s) {
		return errors.New("not a valid metric name")
	}
	return nil
}

func intRange(min, max int64) func(any) error {
	return func(v any) error {
		n, ok := v.(json.Number)
		if !ok {
			return errors.New("must be an integer")
		}
		i, err := n.Int64()
		if err != nil {
			return errors.New("must be an integer")
		}
		if i < min || i > max {
			return fmt.Errorf("must be %d to %d", min, max)
		}
		return nil
	}
}

func numRange(min, max float64) func(any) error {
	return func(v any) error {
		n, ok := v.(json.Number)
		if !ok {
			return errors.New("must be a number")
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("must be a finite number")
		}
		if f < min || f > max {
			return fmt.Errorf("must be %g to %g", min, max)
		}
		return nil
	}
}

func slotCheck(v any) error {
	return objectOf(objSpec{
		required: []string{"slot", "meters"},
		fields: map[string]func(any) error{
			"slot": objectOf(objSpec{
				required: []string{"scope", "kind", "label"},
				fields: map[string]func(any) error{
					"scope": enumOf("user", "domain"),
					"kind":  enumOf("session", "provider"),
					"label": stringLen(1, 80),
				},
			}),
			"headline":       stringLen(0, 128),
			"omitted_groups": intRange(0, math.MaxInt32),
			"meters":         arrayOf(16, meterCheck),
		},
	})(v)
}

func meterCheck(v any) error {
	count := intRange(0, math.MaxInt32)
	return objectOf(objSpec{
		required: []string{"id", "metric", "label", "unit", "observed_at", "expires_at", "coverage"},
		fields: map[string]func(any) error{
			"id":                stringLen(1, 128),
			"metric":            metricName,
			"label":             stringLen(1, 80),
			"unit":              enumOf("percent", "tokens", "requests", "usd", "ms"),
			"used":              numRange(0, math.MaxFloat64),
			"remaining":         numRange(0, math.MaxFloat64),
			"limit":             numRange(0, math.MaxFloat64),
			"used_percent":      numRange(0, 100),
			"remaining_percent": numRange(0, 100),
			"resets_at":         intRange(0, math.MaxInt64),
			"duration_ms":       intRange(1, math.MaxInt64),
			"pace":              enumOf("ahead", "on_pace", "below", "unknown"),
			"observed_at":       intRange(0, math.MaxInt64),
			"expires_at":        intRange(0, math.MaxInt64),
			"coverage": objectOf(objSpec{
				required: []string{"known", "stale", "unknown"},
				fields:   map[string]func(any) error{"known": count, "stale": count, "unknown": count},
			}),
		},
	})(v)
}
