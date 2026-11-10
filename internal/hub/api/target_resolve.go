package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// TargetResolver resolves the human form of a target
// ("worker-name:session-name[:window.pane]", AD-18) to the canonical target,
// inside the space of the authenticated principal; *workers.Router implements
// it. It answers target_not_found for a Worker the principal cannot see and
// conflict for an ambiguous name.
type TargetResolver interface {
	Resolve(ctx context.Context, p authz.Principal, target string, all bool) (ops.Target, error)
}

// SetTargetResolver makes the dispatcher accept the human form of a target.
// With a resolver set, a target member of a Worker operation (a JSON string
// in a body, or ?target=pi-1:minion-3 in a query string) is resolved for the
// authenticated caller before the arguments are decoded; the optional
// boolean member all (an admin with the "mine" filter off) widens the lookup
// to every Worker the caller may read. Without one, only the canonical object
// form is accepted. The resolver is shared by every dispatcher built on r.
func (r *Registry) SetTargetResolver(tr TargetResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = tr
}

func (r *Registry) targetResolver() TargetResolver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.targets
}

// RegisterTargetResolution wires tr into r (see [Registry.SetTargetResolver]).
func RegisterTargetResolution(r *Registry, tr TargetResolver) error {
	if r == nil || tr == nil {
		return invalidArg("", "a registry and a resolver are required")
	}
	r.SetTargetResolver(tr)
	return nil
}

// targetMembers lists the JSON member names of argument type t that hold an
// ops.Target (embedded structs without a tag are flattened).
func targetMembers(t reflect.Type) []string {
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return nil
	}
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		jname, _, _ := strings.Cut(tag, ",")
		switch {
		case f.Anonymous && jname == "" && derefType(f.Type).Kind() == reflect.Struct:
			names = append(names, targetMembers(f.Type)...)
		case !f.IsExported():
		case f.Type == targetType || f.Type == targetPtrType:
			if jname == "" {
				jname = f.Name
			}
			names = append(names, jname)
		}
	}
	return names
}

// humanTargetsFromQuery moves a target given as a query parameter
// (?target=pi-1:minion-3) into obj as a JSON string, and the optional all
// flag with it, so that resolveTargets handles body and query alike. The
// moved names are removed from the returned values, which addValues would
// refuse for an object member.
func humanTargetsFromQuery(op ops.Op, q url.Values, obj rawObject) url.Values {
	names := targetMembers(op.Args)
	var out url.Values
	for _, name := range names {
		vs := q[name]
		if len(vs) != 1 {
			continue
		}
		if out == nil {
			out = url.Values{}
			for k, v := range q {
				out[k] = v
			}
		}
		raw, _ := json.Marshal(vs[0])
		obj[name] = raw
		delete(out, name)
		if all, ok := q["all"]; ok && len(all) == 1 {
			if _, has := fieldTypeAt(op.Args, []string{"all"}); !has {
				flag, _ := json.Marshal(all[0] == "true")
				obj["all"] = flag
			}
		}
	}
	if out == nil {
		return q
	}
	return out
}

// resolveTargets replaces every target member of obj that is a JSON string
// by the canonical object the resolver returns for the caller.
func resolveTargets(ctx context.Context, tr TargetResolver, p authz.Principal, op ops.Op, obj rawObject) error {
	if tr == nil || op.Scope != ops.ScopeWorker {
		return nil
	}
	all := false
	if raw, ok := obj["all"]; ok {
		_ = json.Unmarshal(raw, &all) // a member of another type means false
	}
	for _, name := range targetMembers(op.Args) {
		raw, ok := obj[name]
		if !ok || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return invalidArg(name, "has the wrong type, expected an object or a string")
		}
		t, err := tr.Resolve(ctx, p, s, all)
		if err != nil {
			return err
		}
		b, err := json.Marshal(t)
		if err != nil {
			return invalidArg(name, "cannot be resolved")
		}
		obj[name] = b
	}
	return nil
}
