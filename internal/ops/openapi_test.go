package ops

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestOpenAPIGolden pins the generated description of the Default catalog.
// After a change of an op or of one of its types: go test ./internal/ops
// -run OpenAPI -update, review the diff, then make openapi.
func TestOpenAPIGolden(t *testing.T) {
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	again, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, again) {
		t.Fatal("two runs give different documents")
	}
	golden(t, "openapi.golden.json", string(doc))

	var parsed struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q", parsed.OpenAPI)
	}
	all := All()
	if len(parsed.Paths) != len(all) {
		t.Errorf("%d paths for %d ops", len(parsed.Paths), len(all))
	}
	for _, op := range all {
		item, ok := parsed.Paths[op.RESTPath()]
		if !ok {
			t.Errorf("op %s has no path", op.Name)
			continue
		}
		post, ok := item["post"]
		if !ok {
			t.Errorf("op %s has no POST", op.Name)
			continue
		}
		if post["operationId"] != op.Name || post["summary"] != op.Summary {
			t.Errorf("op %s: operationId %v, summary %v", op.Name, post["operationId"], post["summary"])
		}
		if post["x-minion-scope"] != string(op.Scope) || post["x-minion-write"] != op.Write ||
			post["x-minion-mutates"] != op.Mutates || post["x-minion-stream"] != op.Stream ||
			post["x-minion-session-only"] != op.SessionOnly {
			t.Errorf("op %s: the x-minion members differ from the declaration", op.Name)
		}
		if n := len(post["x-minion-guards"].([]any)); n != len(op.Guards) {
			t.Errorf("op %s: %d guards, want %d", op.Name, n, len(op.Guards))
		}
		if n := len(post["x-minion-actions"].([]any)); n != len(op.Actions) {
			t.Errorf("op %s: %d actions, want %d", op.Name, n, len(op.Actions))
		}
		if _, hasGet := item["get"]; hasGet == op.ChangesState() {
			t.Errorf("op %s: GET present = %v, changes state = %v", op.Name, hasGet, op.ChangesState())
		}
	}
}

// TestOpenAPIRefsResolve follows every $ref of the document.
func TestOpenAPIRefsResolve(t *testing.T) {
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var root any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatal(err)
	}
	refs := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				s, isRef := e.(string)
				if k != "$ref" || !isRef {
					walk(e)
					continue
				}
				refs++
				rest, ok := strings.CutPrefix(s, "#/")
				if !ok {
					t.Errorf("$ref %q is not local", s)
					continue
				}
				cur := root
				for _, part := range strings.Split(rest, "/") {
					part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
					m, _ := cur.(map[string]any)
					if cur, ok = m[part]; !ok {
						t.Errorf("$ref %q does not resolve (no %q)", s, part)
						break
					}
				}
			}
		}
	}
	walk(root)
	if refs == 0 {
		t.Fatal("the document has no $ref")
	}
}

type apiInner struct {
	ID   string    `json:"id"`
	When time.Time `json:"when,omitzero"`
}

type apiEmbedded struct {
	Shared string `json:"shared"`
	Hidden string `json:"-"`
}

type apiProbe struct {
	apiEmbedded
	Name    string              `json:"name"`
	Count   int                 `json:"count,omitempty"`
	Big     int64               `json:"big,string"`
	Ptr     *apiInner           `json:"ptr"`
	At      Timestamp           `json:"at"`
	Raw     json.RawMessage     `json:"raw,omitempty"`
	List    []apiInner          `json:"list"`
	Blob    []byte              `json:"blob,omitempty"`
	ByName  map[string]apiInner `json:"by_name,omitempty"`
	Any     any                 `json:"any,omitempty"`
	Self    *apiProbe           `json:"self,omitempty"`
	NoTag   bool
	private string
}

// TestOpenAPIReflection checks the reflection rules on a type that has one
// member of each kind.
func TestOpenAPIReflection(t *testing.T) {
	_ = apiProbe{}.private
	reg := NewRegistry()
	reg.Register(Op{Name: "probe_read", Summary: "Probe.", Scope: ScopeHub, Actions: MustActionSet(ActionManage),
		Args: reflect.TypeFor[apiProbe](), Result: reflect.TypeFor[apiInner]()})
	doc, err := reg.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Paths      map[string]map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	s, ok := parsed.Components.Schemas["apiProbe"]
	if !ok {
		t.Fatalf("no schema apiProbe in %s", doc)
	}
	if got, want := strings.Join(s.Required, ","), "NoTag,at,big,list,name,shared"; got != want {
		t.Errorf("required = %s, want %s", got, want)
	}
	want := map[string]map[string]any{
		"shared":  {"type": "string"},
		"name":    {"type": "string"},
		"count":   {"type": "integer"},
		"big":     {"type": "string"},
		"ptr":     {"$ref": "#/components/schemas/apiInner"},
		"at":      {"type": "string", "format": "date-time"},
		"raw":     {},
		"list":    {"type": "array", "items": map[string]any{"$ref": "#/components/schemas/apiInner"}},
		"blob":    {"type": "string", "contentEncoding": "base64"},
		"by_name": {"type": "object", "additionalProperties": map[string]any{"$ref": "#/components/schemas/apiInner"}},
		"any":     {},
		"self":    {"$ref": "#/components/schemas/apiProbe"},
		"NoTag":   {"type": "boolean"},
	}
	if len(s.Properties) != len(want) {
		t.Errorf("properties = %v", s.Properties)
	}
	for name, w := range want {
		if got := s.Properties[name]; !reflect.DeepEqual(got, w) {
			t.Errorf("property %s = %v, want %v", name, got, w)
		}
	}
	if w := parsed.Components.Schemas["apiInner"].Properties["when"]; w["format"] != "date-time" {
		t.Errorf("time.Time = %v", w)
	}
	item := parsed.Paths["/api/v1/ops/probe_read"]
	get, ok := item["get"]
	if !ok || get["operationId"] != "probe_read_get" {
		t.Fatalf("GET = %v", get)
	}
	var names []string
	for _, p := range get["parameters"].([]any) {
		names = append(names, p.(map[string]any)["name"].(string))
	}
	if got, want := strings.Join(names, ","), "NoTag,at,big,count,name,ptr.id,ptr.when,shared"; got != want {
		t.Errorf("query parameters = %s, want %s", got, want)
	}
	if got := get["x-minion-query-unsupported"]; !reflect.DeepEqual(got, []any{"any", "blob", "by_name", "list", "raw"}) {
		t.Errorf("x-minion-query-unsupported = %v", got)
	}
}
