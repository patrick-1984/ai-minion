package obs

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOpenTelemetryImportsStayInObs: every import of go.opentelemetry.io/...
// (and of the logr bridge the SDK needs) lives under internal/obs, so that a
// breaking change of a v0 OpenTelemetry module stays local (design §0, T6).
func TestOpenTelemetryImportsStayInObs(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	obsDir := filepath.Join(root, "internal", "obs") + string(filepath.Separator)
	fset := token.NewFileSet()
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if (strings.HasPrefix(p, "go.opentelemetry.io/") || p == "github.com/go-logr/logr") && !strings.HasPrefix(path, obsDir) {
				t.Errorf("%s imports %s; OpenTelemetry imports belong in internal/obs", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("only %d Go files checked; wrong root %s?", checked, root)
	}
}
