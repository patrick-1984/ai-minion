// Command checktmux enforces AD-25 for test code: tests may use tmux only
// through internal/testutil/tmuxtest, which starts it on a private socket.
// It parses every *_test.go file under the given roots (build tags are
// ignored) and reports
//
//   - any string literal that contains the word kill-server, and
//   - any call to exec.Command or exec.CommandContext (or a function named
//     Command or CommandContext) that has a string literal mentioning the
//     word tmux anywhere in its arguments, however deeply the arguments are
//     nested and across however many lines the call spans.
//
// Files below a directory named tmuxtest are exempt. The check is
// syntactic: it cannot see a command name that arrives through a variable,
// which code review has to catch.
//
// Usage: go run ./scripts/checktmux [root ...]   (default: internal test cmd)
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	tmuxWord       = regexp.MustCompile(`(^|[^A-Za-z0-9_-])tmux($|[^A-Za-z0-9_-])`)
	killServerWord = regexp.MustCompile(`kill-server`)
)

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"internal", "test", "cmd"}
	}
	problems, err := check(roots)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-tmux:", err)
		os.Exit(2)
	}
	if len(problems) > 0 {
		fmt.Fprintln(os.Stderr, "check-tmux: tests must use tmux only through internal/testutil/tmuxtest (AD-25):")
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, p)
		}
		os.Exit(1)
	}
	fmt.Println("check-tmux: ok")
}

// check returns one "path:line: message" entry per violation.
func check(roots []string) ([]string, error) {
	var problems []string
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "tmuxtest" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			found, err := checkFile(path)
			problems = append(problems, found...)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(problems)
	return problems, nil
}

func checkFile(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var problems []string
	report := func(pos token.Pos, msg string) {
		problems = append(problems, fmt.Sprintf("%s:%d: %s", path, fset.Position(pos).Line, msg))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING && killServerWord.MatchString(n.Value) {
				report(n.Pos(), "kill-server outside internal/testutil/tmuxtest")
			}
		case *ast.CallExpr:
			if !isCommandCall(n.Fun) {
				return true
			}
			for _, arg := range n.Args {
				if lit := findTmuxLiteral(arg); lit != nil {
					report(n.Pos(), "tmux executed directly in a test; use internal/testutil/tmuxtest")
					break
				}
			}
		}
		return true
	})
	return problems, nil
}

// isCommandCall reports whether fn is exec.Command, exec.CommandContext or a
// plain call of a function with one of those names.
func isCommandCall(fn ast.Expr) bool {
	switch fn := fn.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name == "Command" || fn.Sel.Name == "CommandContext"
	case *ast.Ident:
		return fn.Name == "Command" || fn.Name == "CommandContext"
	}
	return false
}

// findTmuxLiteral returns the first string literal below e that mentions the
// word tmux.
func findTmuxLiteral(e ast.Expr) *ast.BasicLit {
	var found *ast.BasicLit
	ast.Inspect(e, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			s = lit.Value
		}
		if tmuxWord.MatchString(s) {
			found = lit
		}
		return true
	})
	return found
}
