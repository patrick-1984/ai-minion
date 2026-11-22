package presence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func loadSchemaFile(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "schemas", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckUsageAcceptsTheFixtures(t *testing.T) {
	for _, f := range []string{"usage_meters_full.json", "usage_meters_empty.json"} {
		if err := CheckUsage(fixtureBytes(t, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if err := CheckUsage([]byte(usageDoc(1))); err != nil {
		t.Fatal(err)
	}
}

func TestCheckUsageRejects(t *testing.T) {
	full := string(fixtureBytes(t, "usage_meters_full.json"))
	bad := map[string]string{
		"empty":            ``,
		"array":            `[]`,
		"envelope only":    `{"schema":"minion.usage-meters/1"}`,
		"foreign schema":   `{"schema":"x","generated_at":1,"slots":[]}`,
		"trailing value":   usageDoc(1) + ` {}`,
		"trailing bracket": usageDoc(1) + `]`,
		"unknown top":      `{"schema":"minion.usage-meters/1","generated_at":1,"slots":[],"owner":"alice"}`,
		"null slots":       `{"schema":"minion.usage-meters/1","generated_at":1,"slots":null}`,
		"too many slots":   `{"schema":"minion.usage-meters/1","generated_at":1,"slots":[` + strings.TrimSuffix(strings.Repeat(`{"slot":{"scope":"user","kind":"session","label":"a"},"meters":[]},`, 5), ",") + `]}`,
		"unknown nested":   strings.Replace(full, `"meters"`, `"member_ids":["u1"],"meters"`, 1),
		"unknown meter":    strings.Replace(full, `"coverage"`, `"account":"me@example.org","coverage"`, 1),
		"private metric":   strings.Replace(full, `"metric":"anthropic.general.5h"`, `"metric":"Model: secret/Team A"`, 1),
		"oversize":         `{"schema":"minion.usage-meters/1","generated_at":1,"slots":[],"pad":"` + strings.Repeat("a", MaxUsageSize) + `"}`,
	}
	for name, doc := range bad {
		if CheckUsage([]byte(doc)) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every one-step mutation of a valid document is judged the same way by
// CheckUsage and by the published JSON schema: deleting any field, setting it to
// null or to a value of another type or range, and adding an unknown field to
// any object.
func TestUsageValidatorMatchesSchema(t *testing.T) {
	schema := loadSchemaFile(t, "usage-meters.schema.json")
	var base any
	if err := json.Unmarshal(fixtureBytes(t, "usage_meters_full.json"), &base); err != nil {
		t.Fatal(err)
	}
	clone := func() any {
		b, _ := json.Marshal(base)
		var v any
		_ = json.Unmarshal(b, &v)
		return v
	}
	type step struct {
		path []any // string keys and int indexes
	}
	var paths []step
	var walk func(v any, path []any)
	walk = func(v any, path []any) {
		paths = append(paths, step{append([]any(nil), path...)})
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(c, append(path, k))
			}
		case []any:
			for i, c := range x {
				walk(c, append(path, i))
			}
		}
	}
	walk(base, nil)
	get := func(root any, path []any) (parent any, key any) {
		cur := root
		for _, p := range path[:len(path)-1] {
			switch k := p.(type) {
			case string:
				cur = cur.(map[string]any)[k]
			case int:
				cur = cur.([]any)[k]
			}
		}
		return cur, path[len(path)-1]
	}
	mutants := []any{nil, "x", "", -1.0, 1.5, 101.0, true, map[string]any{}, []any{}, strings.Repeat("a", 200)}
	n := 0
	check := func(label string, doc any) {
		n++
		raw, _ := json.Marshal(doc)
		sErr := schema.Validate(doc)
		cErr := CheckUsage(raw)
		if (sErr == nil) != (cErr == nil) {
			t.Errorf("%s: schema err=%v, CheckUsage err=%v\n%s", label, sErr, cErr, raw)
		}
	}
	for _, st := range paths {
		if len(st.path) == 0 {
			continue
		}
		label := ""
		for _, p := range st.path {
			label += "/" + toString(p)
		}
		doc := clone()
		parent, key := get(doc, st.path)
		if m, ok := parent.(map[string]any); ok {
			delete(m, key.(string))
			check("delete "+label, doc)
		}
		for i, mv := range mutants {
			doc := clone()
			parent, key := get(doc, st.path)
			switch p := parent.(type) {
			case map[string]any:
				p[key.(string)] = mv
			case []any:
				p[key.(int)] = mv
			}
			check("set "+label+" #"+toString(i), doc)
		}
	}
	// An unknown field in every object.
	var objs [][]any
	for _, st := range paths {
		if len(st.path) == 0 {
			objs = append(objs, nil)
			continue
		}
		doc := clone()
		parent, key := get(doc, st.path)
		var v any
		switch p := parent.(type) {
		case map[string]any:
			v = p[key.(string)]
		case []any:
			v = p[key.(int)]
		}
		if _, ok := v.(map[string]any); ok {
			objs = append(objs, st.path)
		}
	}
	for _, path := range objs {
		doc := clone()
		if len(path) == 0 {
			doc.(map[string]any)["extra"] = "x"
		} else {
			parent, key := get(doc, path)
			var o map[string]any
			switch p := parent.(type) {
			case map[string]any:
				o = p[key.(string)].(map[string]any)
			case []any:
				o = p[key.(int)].(map[string]any)
			}
			o["extra"] = "x"
		}
		check("unknown field", doc)
	}
	if n < 200 {
		t.Fatalf("only %d mutations", n)
	}
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRecordMatchesSchema(t *testing.T) {
	schema := loadSchemaFile(t, "presence.schema.json")
	var base map[string]any
	if err := json.Unmarshal(fixtureBytes(t, "presence_user_running.json"), &base); err != nil {
		t.Fatal(err)
	}
	mutants := []any{nil, "x", "", -1.0, 0.0, 1.5, 4000.0, true, "connected", "disconnected", "bogus"}
	check := func(label string, m map[string]any) {
		raw, _ := json.Marshal(m)
		_, cErr := DecodeRecord(raw)
		if sErr := schema.Validate(m); (sErr == nil) != (cErr == nil) {
			t.Errorf("%s: schema err=%v, DecodeRecord err=%v\n%s", label, sErr, cErr, raw)
		}
	}
	for k := range base {
		m := clonePresence(base)
		delete(m, k)
		check("delete "+k, m)
		for i, mv := range mutants {
			m := clonePresence(base)
			m[k] = mv
			check("set "+k+" #"+toString(i), m)
		}
	}
	m := clonePresence(base)
	m["extra"] = 1
	check("unknown", m)
}

func clonePresence(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
