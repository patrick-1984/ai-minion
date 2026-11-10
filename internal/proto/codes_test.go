package proto

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var snakeCase = regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)

// required is the list named in Story 1.2.
var required = []string{
	"unsupported_op", "proto_incompatible", "frame_too_large", "not_enrolled", "revoked",
	"unauthenticated", "forbidden", "not_found", "conflict", "rate_limited", "invalid_argument",
	"invite_invalid", "invite_expired", "invite_exhausted", "pairing_code_invalid",
	"pairing_id_collision", "pairing_attempts_exceeded", "already_paired", "worker_offline",
	"target_not_found", "pane_changed", "busy", "shell_pane", "dialog_open", "preview_only",
	"not_shared", "message_too_large", "pipe_rate_limited", "pipe_budget_exceeded",
	"schema_too_new", "untrusted_peer", "internal",
}

func TestRequiredCodesDeclared(t *testing.T) {
	for _, c := range required {
		if !Code(c).Known() {
			t.Errorf("code %q is not declared", c)
		}
	}
}

func TestEveryCodeIsSnakeCaseWithDescriptionAndStatus(t *testing.T) {
	seen := map[Code]bool{}
	for _, c := range Codes() {
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
		if !snakeCase.MatchString(string(c)) {
			t.Errorf("code %q is not snake_case", c)
		}
		d := c.Description()
		if d == "" || strings.Contains(d, "\n") || !strings.HasSuffix(d, ".") {
			t.Errorf("code %q: description %q must be one non-empty sentence line", c, d)
		}
		st := HTTPStatus(c)
		if st < 400 || st > 599 || http.StatusText(st) == "" {
			t.Errorf("code %q: HTTP status %d is not a known error status", c, st)
		}
	}
	if len(seen) < len(required) {
		t.Fatalf("only %d codes", len(seen))
	}
}

func TestHTTPStatusSelected(t *testing.T) {
	want := map[Code]int{
		CodeUnauthenticated: 401, CodeForbidden: 403, CodeNotFound: 404, CodeConflict: 409,
		CodeRateLimited: 429, CodeInvalidArgument: 400, CodeFrameTooLarge: 413, CodeInternal: 500,
	}
	for c, s := range want {
		if got := HTTPStatus(c); got != s {
			t.Errorf("HTTPStatus(%s) = %d, want %d", c, got, s)
		}
	}
	if HTTPStatus("no_such_code") != 500 || Code("no_such_code").Known() || Code("no_such_code").Description() != "" {
		t.Error("unknown code handling")
	}
}

// TestConstantsMatchTable parses codes.go and checks that the set of constants
// declared with type Code equals the set of values in the registry returned by
// Codes(): every constant has a table row, every row has a constant, and no
// two constants share a value.
func TestConstantsMatchTable(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "codes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{} // value -> first constant name
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Code" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					t.Errorf("constant %s has no value", name.Name)
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("constant %s must be a string literal", name.Name)
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Errorf("constant %s: %v", name.Name, err)
					continue
				}
				if prev, dup := consts[val]; dup {
					t.Errorf("constants %s and %s share the value %q", prev, name.Name, val)
					continue
				}
				consts[val] = name.Name
			}
		}
	}
	if len(consts) == 0 {
		t.Fatal("no Code constants found in codes.go")
	}
	table := map[string]bool{}
	for _, c := range Codes() {
		table[string(c)] = true
		if _, ok := consts[string(c)]; !ok {
			t.Errorf("table entry %q has no Code constant", c)
		}
	}
	for val, name := range consts {
		if !table[val] {
			t.Errorf("constant %s (%q) has no row in the code table", name, val)
		}
		if !Code(val).Known() || HTTPStatus(Code(val)) == 500 && val != "internal" {
			t.Errorf("constant %s (%q) is not resolvable through Known/HTTPStatus", name, val)
		}
	}
}
