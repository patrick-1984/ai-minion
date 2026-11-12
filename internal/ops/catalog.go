package ops

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Scope tells which side of the product executes an operation.
type Scope string

const (
	// ScopeWorker operations run on a Worker. The Hub routes them.
	ScopeWorker Scope = "worker"
	// ScopeHub operations run on the Hub.
	ScopeHub Scope = "hub"
)

// RESTPrefix is the path under which every operation is served.
const RESTPrefix = "/api/v1/ops/"

// Op is one declaration of the catalog (AD-2). REST routes, wire op names, MCP
// tool names and CLI subcommands are derived from it, so no surface can name,
// type or permission the action differently.
type Op struct {
	// Name is the snake_case name, shared by every surface.
	Name string
	// Summary is one English sentence, used for MCP tool and CLI help texts.
	Summary string
	// Scope tells whether the Worker or the Hub executes the op.
	Scope Scope
	// Actions is the accepted action set: any one of its actions suffices.
	Actions ActionSet
	// Write marks ops that change state. A write op has WriteArgs in its
	// arguments, returns WriteResult and declares its Guards.
	Write bool
	// Mutates marks a state-changing op that cannot be a Write op because its
	// result is not a WriteResult (it returns a value that is shown once, such
	// as a new key). It is POST-only like a write, so that a cross-site GET
	// cannot trigger it, and its handler audits it. A Write op needs no flag.
	Mutates bool
	// SessionOnly marks a Hub op that only a browser session may run, and only
	// after a fresh re-authentication (the passphrase operations of the Hub
	// identity). An API key, an act-as session and the generic command line
	// are refused: producers derive no CLI command for it, and the REST
	// dispatcher answers an API key with forbidden. It requires a Mutates op.
	SessionOnly bool
	// Stream marks ops whose result is a stream of Result values.
	Stream bool
	// Guards lists the action-contract class of each guard of a write op.
	Guards []Guard
	// Args is the Go argument type (a struct). Its JSON encoding is the wire
	// form.
	Args reflect.Type
	// Result is the Go result type (a struct). For a streaming op it is the
	// type of one stream element.
	Result reflect.Type
}

// ChangesState reports whether the op changes state: a Write op or a Mutates
// op. Every generic gate that treats a state change differently (the read-only
// MCP server, its read-only hint, the retry and unknown-outcome rules of a
// client, the POST-only routes) asks this, never Write alone: an op that
// Mutates is as much a change as one that Writes.
func (o Op) ChangesState() bool { return o.Write || o.Mutates }

// NewArgs returns a pointer to a zero value of the argument type, for
// decoding a request.
func (o Op) NewArgs() any { return reflect.New(o.Args).Interface() }

// NewResult returns a pointer to a zero value of the result type.
func (o Op) NewResult() any { return reflect.New(o.Result).Interface() }

// WireName is the op name in `op.request` frames.
func (o Op) WireName() string { return o.Name }

// MCPTool is the MCP tool name.
func (o Op) MCPTool() string { return o.Name }

// CLICommand is the CLI subcommand name (`minion <name>`, `mn <name>`).
func (o Op) CLICommand() string { return o.Name }

// RESTPath is the route path, `/api/v1/ops/<name>`.
func (o Op) RESTPath() string { return RESTPrefix + o.Name }

// RESTMethods lists the HTTP methods of the route: POST for every op, and GET
// in addition for ops that neither write nor mutate.
func (o Op) RESTMethods() []string {
	if !o.ChangesState() {
		return []string{"POST", "GET"}
	}
	return []string{"POST"}
}

// RESTRoute is the route in "METHOD path" form for POST, the canonical one.
func (o Op) RESTRoute() string { return "POST " + o.RESTPath() }

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

func isSnakeCase(s string) bool { return snakeCase.MatchString(s) }

// validate returns the first problem of the declaration, or nil.
func (o Op) validate() error {
	if !isSnakeCase(o.Name) {
		return fmt.Errorf("name %q is not snake_case", o.Name)
	}
	if o.Summary == "" {
		return fmt.Errorf("op %s has no summary", o.Name)
	}
	if o.Scope != ScopeWorker && o.Scope != ScopeHub {
		return fmt.Errorf("op %s has unknown scope %q", o.Name, o.Scope)
	}
	if _, err := NewActionSet(o.Actions...); err != nil {
		return fmt.Errorf("op %s: %w", o.Name, err)
	}
	if !slices.IsSorted(o.Actions) {
		return fmt.Errorf("op %s: accepted action set is not canonical, build it with NewActionSet", o.Name)
	}
	if o.Args == nil || o.Args.Kind() != reflect.Struct {
		return fmt.Errorf("op %s: argument type must be a struct", o.Name)
	}
	if o.Result == nil || o.Result.Kind() != reflect.Struct {
		return fmt.Errorf("op %s: result type must be a struct", o.Name)
	}
	if o.SessionOnly && (!o.Mutates || o.Scope != ScopeHub) {
		return fmt.Errorf("op %s: SessionOnly is for Hub ops that Mutate", o.Name)
	}
	if o.Stream && (o.Write || o.Mutates) {
		return fmt.Errorf("op %s: a streaming op cannot be a write", o.Name)
	}
	if o.Write && o.Mutates {
		return fmt.Errorf("op %s: Mutates is for state-changing ops that are not Write ops", o.Name)
	}
	if !o.Write {
		if len(o.Guards) > 0 {
			return fmt.Errorf("op %s: only write ops declare guards", o.Name)
		}
		return nil
	}
	if len(o.Guards) == 0 {
		return fmt.Errorf("write op %s declares no FORCE/KEEP/WARN guard list", o.Name)
	}
	seen := map[string]bool{}
	for _, g := range o.Guards {
		if err := g.validate(); err != nil {
			return fmt.Errorf("op %s: %w", o.Name, err)
		}
		if seen[g.Name] {
			return fmt.Errorf("op %s: duplicate guard %q", o.Name, g.Name)
		}
		seen[g.Name] = true
	}
	fields := jsonFields(o.Args)
	if f, ok := fields["request_id"]; !ok || f.Kind() != reflect.String {
		return fmt.Errorf("write op %s: arguments need a string field request_id", o.Name)
	}
	if f, ok := fields["force"]; !ok || f.Kind() != reflect.Bool {
		return fmt.Errorf("write op %s: arguments need a bool field force", o.Name)
	}
	if o.Result != reflect.TypeFor[WriteResult]() {
		return fmt.Errorf("write op %s: result type must be WriteResult", o.Name)
	}
	return nil
}

// jsonFields returns the JSON object members of a struct type with their Go
// types, following the encoding/json rules: embedded structs without a tag
// are flattened, `json:"-"` fields are skipped, and a name declared at several
// depths resolves to the shallowest one. Among equally shallow declarations a
// single tagged one wins; otherwise the name is ambiguous and encoding/json
// drops it, so it is absent from the result.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	type cand struct {
		typ    reflect.Type
		depth  int
		tagged bool
	}
	all := map[string][]cand{}
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
			name, _, _ := strings.Cut(tag, ",")
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
			tagged := name != ""
			if name == "" {
				name = f.Name
			}
			all[name] = append(all[name], cand{f.Type, depth, tagged})
		}
	}
	walk(t, 0, map[reflect.Type]bool{})
	out := map[string]reflect.Type{}
	for name, cs := range all {
		minDepth := cs[0].depth
		for _, c := range cs {
			minDepth = min(minDepth, c.depth)
		}
		var top []cand
		for _, c := range cs {
			if c.depth == minDepth {
				top = append(top, c)
			}
		}
		if len(top) > 1 {
			var tagged []cand
			for _, c := range top {
				if c.tagged {
					tagged = append(tagged, c)
				}
			}
			if len(tagged) != 1 {
				continue // ambiguous: encoding/json drops the field
			}
			top = tagged
		}
		out[name] = top[0].typ
	}
	return out
}

// Registry holds the declarations. The package-level Register, Lookup and All
// use Default; tests create their own with NewRegistry.
type Registry struct {
	mu  sync.RWMutex
	ops map[string]Op
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{ops: map[string]Op{}} }

// Default is the process-wide catalog that every story registers into.
var Default = NewRegistry()

// Register adds op. It panics, so that a wrong declaration fails at init, when
// the name is not snake_case or already registered, the accepted action set is
// invalid, the types are not structs, or a write op lacks its guard list, its
// request_id and force arguments or the WriteResult result.
func (r *Registry) Register(op Op) {
	if err := op.validate(); err != nil {
		panic("ops: Register: " + err.Error())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.ops[op.Name]; dup {
		panic(fmt.Sprintf("ops: Register: op %q is already registered", op.Name))
	}
	r.ops[op.Name] = op.clone()
}

// clone returns a copy that shares no slice memory with o.
func (o Op) clone() Op {
	o.Actions = slices.Clone(o.Actions)
	o.Guards = slices.Clone(o.Guards)
	return o
}

// Lookup returns the op with the given name. The returned Op is a copy; changing
// its Actions or Guards does not affect the registry.
func (r *Registry) Lookup(name string) (Op, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	op, ok := r.ops[name]
	if ok {
		op = op.clone()
	}
	return op, ok
}

// All returns every op sorted by name, so that every derived list is
// deterministic whatever the init order.
func (r *Registry) All() []Op {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Op, 0, len(r.ops))
	for _, op := range r.ops {
		out = append(out, op.clone())
	}
	slices.SortFunc(out, func(a, b Op) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Register adds op to the Default catalog; see Registry.Register.
func Register(op Op) { Default.Register(op) }

// Lookup finds an op in the Default catalog.
func Lookup(name string) (Op, bool) { return Default.Lookup(name) }

// All lists the Default catalog sorted by name.
func All() []Op { return Default.All() }

// Select returns the ops of the Default catalog with the given names, in the
// order given. It panics on an unknown name.
func Select(names ...string) []Op {
	out := make([]Op, 0, len(names))
	for _, n := range names {
		op, ok := Lookup(n)
		if !ok {
			panic(fmt.Sprintf("ops: Select: unknown op %q", n))
		}
		out = append(out, op)
	}
	return out
}

// DerivedTable renders the names every surface derives from the given ops, one
// line per op: name, REST methods and path, wire op name, MCP tool, CLI
// subcommand. A golden file test pins it.
func DerivedTable(list []Op) string {
	var b strings.Builder
	for _, op := range list {
		fmt.Fprintf(&b, "%s | rest %s %s | wire %s | mcp %s | cli %s\n",
			op.Name, strings.Join(op.RESTMethods(), ","), op.RESTPath(), op.WireName(), op.MCPTool(), op.CLICommand())
	}
	return b.String()
}

// CatalogTable renders the contract of the given ops: scope, accepted actions,
// write and stream flags, the argument and result types and the guards with
// their classes. A golden file test pins it.
func CatalogTable(list []Op) string {
	var b strings.Builder
	for _, op := range list {
		guards := make([]string, len(op.Guards))
		for i, g := range op.Guards {
			guards[i] = g.Name + ":" + string(g.Class) + "(" + g.Reads + ")"
		}
		g := "-"
		if len(guards) > 0 {
			g = strings.Join(guards, ",")
		}
		fmt.Fprintf(&b, "%s | %s | actions %s | write=%t stream=%t | args %s | result %s | guards %s\n",
			op.Name, op.Scope, op.Actions, op.Write, op.Stream, op.Args.Name(), op.Result.Name(), g)
	}
	return b.String()
}
