// Package filexfer is the transfer engine of files.v1 (BRD minion-file-transfer
// v2): wire paths, the folder walk, the restricted pax tar format, the content
// codings, the canonical manifest, staging, verification and the no-replace
// publication of received files, limits and the cleanup of stale staging
// files. Confinement (the pinned root handle, the per-OS resolvers and the
// identity sets) is provided by the root files of this package (WP2); the
// engine reaches the file system only through the Root interface (walk.go)
// and through file descriptors of directories opened by it.
//
// Nothing in this package builds an absolute path from a wire path, follows a
// symbolic link, sets an exec bit or executes content.
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

// Names that are push-protected anywhere in a path (FR-8): a directory with
// this name protects everything below it, a file with this name is protected
// itself. Matching is ASCII case-insensitive, so that ".Git/hooks" on a
// case-insensitive volume is protected too.
var pushProtectedNames = map[string]bool{
	".hg": true, ".svn": true, ".envrc": true, ".bashrc": true, ".bash_profile": true,
	".profile": true, ".zshrc": true, ".zprofile": true, ".zshenv": true, "config.fish": true,
	".ssh": true, ".gnupg": true, ".npmrc": true, ".pypirc": true, ".netrc": true,
	".claude": true, ".mcp.json": true, ".codex": true, ".cursor": true, ".gitlab-ci.yml": true,
}

// pushProtectedPairs are protected only as a directory followed by one of the
// names (".git/hooks", ".git/config", ...).
var pushProtectedPairs = map[string]map[string]bool{
	".git":    {"hooks": true, "config": true},
	".github": {"workflows": true},
	".vscode": {"tasks.json": true, "settings.json": true},
}

// PushProtected reports whether a push may not write rel (a wire path relative
// to the receiving root) because of the default push-protected name list of
// FR-8. Protection by directory identity (a root inside .git/hooks) and the
// Worker state directory are checked by the identity sets, not here. The
// Worker owner's file_transfer_protected=off is applied by the caller.
func PushProtected(rel string) bool {
	elems := strings.Split(strings.ToLower(rel), "/")
	for i, e := range elems {
		if pushProtectedNames[e] || strings.HasPrefix(e, ".env") {
			return true
		}
		if next, ok := pushProtectedPairs[e]; ok && i+1 < len(elems) && next[elems[i+1]] {
			return true
		}
	}
	return false
}

// pullSensitiveNames are pull-sensitive anywhere in a path (FR-8).
var pullSensitiveNames = map[string]bool{
	".ssh": true, ".gnupg": true, ".aws": true, ".netrc": true, ".git-credentials": true,
}

// PullSensitive reports whether rel (a wire path relative to the sending root)
// is pull-sensitive by the default name list of FR-8: a folder pull skips it
// with proto.SkipSensitive, a direct pull refuses it with
// proto.CodePathSensitive. ".git" is pullable (Q5). The Minion state
// directory is checked by identity, not here.
func PullSensitive(rel string) bool {
	for _, e := range strings.Split(strings.ToLower(rel), "/") {
		if pullSensitiveNames[e] {
			return true
		}
		if strings.HasPrefix(e, "id_") && !strings.HasSuffix(e, ".pub") {
			return true
		}
		if strings.HasSuffix(e, ".pem") || strings.HasSuffix(e, ".key") {
			return true
		}
	}
	return false
}
