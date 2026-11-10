package version

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestNoHTTPOutsideUpdate keeps AD-19 true: the binary contacts the release
// host only during "minion update". No file of this package except update.go
// may import net/http (or another client library), and update.go creates a
// client in exactly one function, which only NewGitHubSource (the release host)
// and newTUFFetcher (the Sigstore TUF repository) call. No
// package-level variable or init function may build one.
func TestNoHTTPOutsideUpdate(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	forbidden := map[string]bool{"net/http": true, "net/http/httputil": true, "net/rpc": true, "golang.org/x/net/http2": true}
	var clientBuilders, newHTTPClientCalls []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if forbidden[path] && name != "update.go" {
				t.Errorf("%s imports %s; only update.go may", name, path)
			}
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				// Package-level variables must not build a client.
				ast.Inspect(d, func(n ast.Node) bool {
					if usesHTTP(n) && d.Tok == token.VAR {
						t.Errorf("%s: package-level variable uses net/http", name)
					}
					return true
				})
			case *ast.FuncDecl:
				if d.Name.Name == "init" {
					ast.Inspect(d, func(n ast.Node) bool {
						if usesHTTP(n) {
							t.Errorf("%s: init uses net/http", name)
						}
						return true
					})
				}
				ast.Inspect(d, func(n ast.Node) bool {
					if cl, ok := n.(*ast.CompositeLit); ok && isSel(cl.Type, "http", "Client") {
						clientBuilders = append(clientBuilders, name+":"+d.Name.Name)
					}
					if sel, ok := n.(*ast.SelectorExpr); ok && isSel(sel, "http", "DefaultClient") {
						t.Errorf("%s uses http.DefaultClient", name)
					}
					if call, ok := n.(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "newHTTPClient" {
							newHTTPClientCalls = append(newHTTPClientCalls, d.Name.Name)
						}
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isSel(sel, "http", "Get", "Post", "Head", "PostForm") {
							t.Errorf("%s calls http.%s", name, sel.Sel.Name)
						}
					}
					return true
				})
			}
		}
	}
	if len(clientBuilders) != 1 || clientBuilders[0] != "update.go:newHTTPClient" {
		t.Errorf("HTTP clients are built in %v, want only update.go:newHTTPClient", clientBuilders)
	}
	// The two outbound destinations of an online update: the release host
	// and the Sigstore TUF repository.
	sort.Strings(newHTTPClientCalls)
	if got := strings.Join(newHTTPClientCalls, ","); got != "NewGitHubSource,newTUFFetcher" {
		t.Errorf("newHTTPClient is called from %v, want only NewGitHubSource and newTUFFetcher", newHTTPClientCalls)
	}
}

func usesHTTP(n ast.Node) bool {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http"
}

func isSel(e ast.Expr, pkg string, names ...string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != pkg {
		return false
	}
	for _, n := range names {
		if sel.Sel.Name == n {
			return true
		}
	}
	return false
}
