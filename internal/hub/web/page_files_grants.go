package web

// The file transfer flags of the grant dialog (files.v1, WP9): two
// checkboxes of the Add dialog, unchecked by default. A grant with either
// flag is named in the confirmation sentence and saved only after the
// person confirms it; the Hub refuses files_push below level control and
// file flags on a source that is not verified.

import (
	"net/url"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// grantFileFlags sets the file flags of a new grant from the form. It returns
// the sentence of a refusal, empty when the flags can be sent.
func grantFileFlags(a *ops.GrantSetArgs, f url.Values) string {
	pull, push := f.Get("files_pull") == "1", f.Get("files_push") == "1"
	if push && a.Level != ops.GrantLevelControl {
		return "Pushing files needs the level Control."
	}
	if pull {
		a.FilesPull = &pull
	}
	if push {
		a.FilesPush = &push
	}
	return ""
}

// fileFlagsKey is the part of the confirmation key that the file flags
// decide: empty without a flag.
func fileFlagsKey(a ops.GrantSetArgs) string {
	pull := a.FilesPull != nil && *a.FilesPull
	push := a.FilesPush != nil && *a.FilesPush
	switch {
	case pull && push:
		return "files:pull,push"
	case pull:
		return "files:pull"
	case push:
		return "files:push"
	}
	return ""
}

// fileFlagsSentence is the addition to the confirmation sentence.
func fileFlagsSentence(a ops.GrantSetArgs) string {
	switch fileFlagsKey(a) {
	case "files:pull,push":
		return " It may also copy files out of the target session's folder and into it."
	case "files:pull":
		return " It may also copy files out of the target session's folder."
	case "files:push":
		return " It may also copy files into the target session's folder."
	}
	return ""
}
