package filexfer

import (
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// CheckPath checks a wire path (BRD FR-5): ops.CheckFilePath plus NFC
// normalization. The empty path names the root and is accepted only when root
// is true. A refusal is a *proto.Error with proto.CodePathOutsideRoot whose
// message never contains the path.
func CheckPath(p string, root bool) error {
	if err := ops.CheckFilePath(p, root); err != nil {
		return err
	}
	if !norm.NFC.IsNormalString(p) {
		return proto.NewError(proto.CodePathOutsideRoot, "the path is not in Unicode normalization form C")
	}
	return nil
}

// validName reports whether name (one directory entry read from disk) can be
// a wire path element: CheckPath of the single element passes.
func validName(name string) bool {
	return !strings.Contains(name, "/") && CheckPath(name, false) == nil
}

// JoinPath joins two wire paths; an empty side is left out.
func JoinPath(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "/" + b
}

// splitParent returns the parent of a non-empty wire path ("" for a top-level
// name) and its last element.
func splitParent(p string) (parent, name string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// BaseName returns the last element of a wire path ("" for the root).
func BaseName(p string) string {
	_, name := splitParent(p)
	return name
}
