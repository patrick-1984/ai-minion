package ops

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// OpenAPIPath is the route that serves the generated description of the API.
const OpenAPIPath = "/api/v1/ops"

// OpenAPI returns the OpenAPI 3.1 description of the Default catalog; see
// [Registry.OpenAPI].
func OpenAPI() ([]byte, error) { return Default.OpenAPI() }

// OpenAPI returns the OpenAPI 3.1 description of the catalog as indented JSON
// that ends with a newline. It is generated from the declarations alone: one
// path /api/v1/ops/<name> per op with POST, and GET for an op that does not
// change state; the request body is the reflection of the argument type and
// the 200 answer the reflection of the result type; the x-minion-* members
// repeat the declaration. Every map is written with sorted keys, so the same
// catalog always gives the same bytes.
//
// The reflection follows encoding/json: JSON tags name the members, embedded
// structs are flattened, a member with omitempty or omitzero, or of a pointer
// type, is optional. Times are strings in date-time format, json.RawMessage
// and interfaces are any value. A struct type that has its own MarshalJSON is
// described by its fields (the catalog's marshalers only keep arrays from
// being null and write embedded members flat). The values an enumeration
// accepts are not known by reflection: such a member is a plain string.
func (r *Registry) OpenAPI() ([]byte, error) {
	g := &apiGen{schemas: map[string]any{}, names: map[reflect.Type]string{}, owner: map[string]reflect.Type{}}
	paths := map[string]any{}
	errRefs, responses := errorResponses()
	for _, op := range r.All() {
		item := map[string]any{"post": g.operation(op, "POST", errRefs)}
		if !op.ChangesState() {
			item["get"] = g.operation(op, "GET", errRefs)
		}
		paths[op.RESTPath()] = item
	}
	codes := make([]any, 0, len(proto.Codes()))
	for _, c := range proto.Codes() {
		codes = append(codes, string(c))
	}
	g.schemas["ErrorCode"] = map[string]any{"type": "string", "enum": codes,
		"description": "Every error code; the HTTP status is fixed per code."}
	g.schemas["Error"] = map[string]any{"type": "object", "required": []any{"code", "message", "details"},
		"properties": map[string]any{
			"code":    ref("ErrorCode"),
			"message": map[string]any{"type": "string"},
			"details": map[string]any{"type": "object", "description": "Always an object, {} when there is nothing."},
		}}
	g.schemas["ErrorEnvelope"] = map[string]any{"type": "object", "required": []any{"error"},
		"description": "The only error shape of the API.",
		"properties":  map[string]any{"error": ref("Error")}}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "Minion Hub API",
			"version": "v1",
			"description": "Generated from the operation catalog (internal/ops); do not edit by hand. " +
				"Every operation is served at POST " + RESTPrefix + "<name>, and at GET with the arguments " +
				"in the query string when it does not change state. A success answer is the bare result " +
				"with HTTP 200; every failure is the error envelope with the HTTP status of its code.",
		},
		"security": []any{
			map[string]any{"bearerApiKey": []any{}},
			map[string]any{"sessionCookie": []any{}, "csrfHeader": []any{}},
		},
		"paths": paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerApiKey": map[string]any{"type": "http", "scheme": "bearer",
					"description": "Authorization: Bearer mn_... (an API key)."},
				"sessionCookie": map[string]any{"type": "apiKey", "in": "cookie", "name": "minion_session",
					"description": "Browser session cookie."},
				"csrfHeader": map[string]any{"type": "apiKey", "in": "header", "name": "X-Minion-CSRF",
					"description": "CSRF token of the session; required with the cookie on every method other than GET."},
			},
			"responses": responses,
			"schemas":   g.schemas,
		},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("ops: OpenAPI: %w", err)
	}
	return buf.Bytes(), nil
}

func ref(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

// errorResponses builds one shared response per HTTP status of the error
// codes, and the responses member that every operation refers to them with.
func errorResponses() (refs map[string]any, components map[string]any) {
	byStatus := map[int][]any{}
	for _, c := range proto.Codes() {
		st := proto.HTTPStatus(c)
		byStatus[st] = append(byStatus[st], string(c))
	}
	refs, components = map[string]any{}, map[string]any{}
	for st, codes := range byStatus {
		name := "Error" + strconv.Itoa(st)
		list := make([]string, len(codes))
		for i, c := range codes {
			list[i] = c.(string)
		}
		components[name] = map[string]any{
			"description": "Error envelope with one of the codes: " + strings.Join(list, ", ") + ".",
			"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"allOf": []any{ref("ErrorEnvelope"), map[string]any{"properties": map[string]any{
					"error": map[string]any{"properties": map[string]any{"code": map[string]any{"enum": codes}}},
				}}},
			}}},
		}
		refs[strconv.Itoa(st)] = map[string]any{"$ref": "#/components/responses/" + name}
	}
	return refs, components
}

// apiGen reflects Go types into JSON Schema. A named struct type becomes a
// component that is referred to; everything else is written in place.
type apiGen struct {
	schemas map[string]any
	names   map[reflect.Type]string
	owner   map[string]reflect.Type
}

// operation describes one method of the route of op.
func (g *apiGen) operation(op Op, method string, errRefs map[string]any) map[string]any {
	guards := make([]any, 0, len(op.Guards))
	for _, gd := range op.Guards {
		guards = append(guards, map[string]any{"name": gd.Name, "class": string(gd.Class), "reads": gd.Reads})
	}
	actions := make([]any, 0, len(op.Actions))
	for _, a := range op.Actions {
		actions = append(actions, string(a))
	}
	okType := "application/json"
	okText := "The result of the operation."
	if op.Stream {
		okType, okText = "application/x-ndjson", "A stream of results, one JSON document per line."
	}
	responses := map[string]any{"200": map[string]any{
		"description": okText,
		"content":     map[string]any{okType: map[string]any{"schema": g.schema(op.Result)}},
	}}
	for st, r := range errRefs {
		responses[st] = r
	}
	o := map[string]any{
		"operationId":           op.Name,
		"summary":               op.Summary,
		"responses":             responses,
		"x-minion-scope":        string(op.Scope),
		"x-minion-actions":      actions,
		"x-minion-write":        op.Write,
		"x-minion-mutates":      op.Mutates,
		"x-minion-stream":       op.Stream,
		"x-minion-session-only": op.SessionOnly,
		"x-minion-guards":       guards,
	}
	if op.Unreleased {
		o["x-minion-unreleased"] = true
	}
	args := g.schema(op.Args)
	fields := structFields(op.Args)
	var hidden []any
	for _, f := range fields {
		if HiddenMember(op.Name, f.name) {
			hidden = append(hidden, f.name)
		}
	}
	if hidden != nil {
		// Members the Hub sets itself; a caller must not send them.
		o["x-minion-hidden-members"] = hidden
	}
	switch method {
	case "GET":
		o["operationId"] = op.Name + "_get"
		// A GET request with the session cookie needs no CSRF header.
		o["security"] = []any{map[string]any{"bearerApiKey": []any{}}, map[string]any{"sessionCookie": []any{}}}
		params, unsupported := []any{}, []any{}
		g.queryParams("", op.Name, op.Args, map[reflect.Type]bool{}, &params, &unsupported)
		o["parameters"] = params
		if len(unsupported) > 0 {
			o["x-minion-query-unsupported"] = unsupported
		}
	default:
		o["requestBody"] = map[string]any{
			"required": len(fields) > 0,
			"content":  map[string]any{"application/json": map[string]any{"schema": args}},
		}
		if op.SessionOnly {
			o["security"] = []any{map[string]any{"sessionCookie": []any{}, "csrfHeader": []any{}}}
		}
	}
	return o
}

// queryParams lists the argument members that the GET route reads from the
// query string: scalars and lists of scalars, the members of a nested object
// with a dotted name. Any other member needs POST and is named in unsupported.
func (g *apiGen) queryParams(prefix, op string, t reflect.Type, path map[reflect.Type]bool, params, unsupported *[]any) {
	if path[t] {
		return
	}
	path[t] = true
	defer delete(path, t)
	for _, f := range structFields(t) {
		if prefix == "" && HiddenMember(op, f.name) {
			continue
		}
		ft := f.typ
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		name := prefix + f.name
		elem := ft
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.Uint8 {
			elem = ft.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
		}
		switch {
		case isScalar(elem):
			*params = append(*params, map[string]any{"name": name, "in": "query", "required": false, "schema": g.schema(ft)})
		case ft.Kind() == reflect.Struct:
			g.queryParams(name+".", op, ft, path, params, unsupported)
		default:
			*unsupported = append(*unsupported, name)
		}
	}
}

var (
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	rawMessageType      = reflect.TypeFor[json.RawMessage]()
	timeType            = reflect.TypeFor[time.Time]()
	timestampType       = reflect.TypeFor[Timestamp]()
)

// isScalar reports whether a query-string value can fill a member of type t.
func isScalar(t reflect.Type) bool {
	switch {
	case t == rawMessageType:
		return false
	case t == timeType || t == timestampType:
		return true
	case t.Implements(jsonUnmarshalerType) || reflect.PointerTo(t).Implements(jsonUnmarshalerType):
		// The route hands the value to the type as a JSON string.
		return t.Kind() != reflect.Struct
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// schema returns the JSON Schema of the JSON form of t.
func (g *apiGen) schema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType || t == timestampType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawMessageType:
		return map[string]any{}
	case t.Kind() != reflect.Struct && (t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType)):
		return map[string]any{}
	case t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType):
		return map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer", "minimum": 0}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schema(t.Elem())}
	case reflect.Struct:
		if t.Name() == "" {
			return g.object(t)
		}
		name, ok := g.names[t]
		if !ok {
			name = g.componentName(t)
			g.names[t] = name
			g.owner[name] = t
			g.schemas[name] = map[string]any{} // reserved: a type may contain itself
			g.schemas[name] = g.object(t)
		}
		return ref(name)
	}
	return map[string]any{} // an interface: any value
}

// componentName is the type name; a second type of the same name from another
// package gets the package name in front.
func (g *apiGen) componentName(t reflect.Type) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r == '.' || r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
				return r
			}
			return '_'
		}, s)
	}
	name := clean(t.Name())
	if _, taken := g.owner[name]; !taken {
		return name
	}
	pkg := t.PkgPath()
	name = clean(pkg[strings.LastIndex(pkg, "/")+1:]) + "." + name
	for i := 2; ; i++ {
		if _, taken := g.owner[name]; !taken {
			return name
		}
		name = name + "_" + strconv.Itoa(i)
	}
}

// object is the schema of the members of a struct type.
func (g *apiGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	required := []any{}
	for _, f := range structFields(t) {
		s := g.schema(f.typ)
		if f.quoted {
			s = map[string]any{"type": "string"}
		}
		props[f.name] = s
		if !f.optional {
			required = append(required, f.name)
		}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// apiField is one JSON member of a struct type.
type apiField struct {
	name     string
	typ      reflect.Type
	optional bool // omitempty, omitzero or a pointer, unless tagged openapi:"required"
	quoted   bool // the ",string" option
}

// structFields lists the JSON members of a struct type sorted by name. Which
// names exist, and with which type, is decided by jsonFields (the rules of
// encoding/json); this adds the tag options of the declaration that won.
func structFields(t reflect.Type) []apiField {
	visible := jsonFields(t)
	best := map[string]apiField{}
	depthOf := map[string]int{}
	var walk func(t reflect.Type, depth int, path map[reflect.Type]bool)
	walk = func(t reflect.Type, depth int, path map[reflect.Type]bool) {
		if path[t] {
			return
		}
		path[t] = true
		defer delete(path, t)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft, depth+1, path)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if visible[name] != f.Type {
				continue
			}
			if d, seen := depthOf[name]; seen && d <= depth {
				continue
			}
			o := strings.Split(opts, ",")
			depthOf[name] = depth
			optional := f.Type.Kind() == reflect.Pointer || slices.Contains(o, "omitempty") || slices.Contains(o, "omitzero")
			best[name] = apiField{
				name:     name,
				typ:      f.Type,
				optional: optional && f.Tag.Get("openapi") != "required",
				quoted:   slices.Contains(o, "string"),
			}
		}
	}
	walk(t, 0, map[reflect.Type]bool{})
	out := make([]apiField, 0, len(best))
	for _, f := range best {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b apiField) int { return strings.Compare(a.name, b.name) })
	return out
}
