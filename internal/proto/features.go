package proto

import (
	"regexp"
	"sort"
)

// Worker features (spine AM-13, EPICS A-26).
//
// A feature is a behaviour of a Worker that cannot be seen in the op names it
// announces: a Worker that predates a safeguard announces the same ops as one
// that has it, and it ignores every argument field it does not know (AD-4), so
// a request that relies on the new field is silently weakened by it. The Worker
// therefore lists the features it really implements in its hello, which is part
// of the handshake transcript that both peers sign (AD-6); the Hub keeps the
// list for the connection and routes a request that needs a feature only to a
// Worker that announced it. There is no version inference: development builds
// share one version string.
const (
	// FeatureAnswerReviewed: answer enforces the reviewed dialog (the argument
	// expect: pane id, pid, command, session generation and dialog digest) under
	// the pane lock and refuses with pane_changed or dialog_changed, never
	// forceable.
	FeatureAnswerReviewed = "answer.reviewed"
	// FeatureSendTextSubmit: send_text honours the argument submit, pasting and
	// pressing Enter in one request under one pane lock and one expected pane.
	FeatureSendTextSubmit = "send_text.submit"
	// FeatureTextVerbatim: send_text and pipe_send deliver text byte for byte
	// (LF stays LF, bracketed paste markers only when the program enabled them)
	// and refuse the control characters that a paste would turn into input.
	FeatureTextVerbatim = "text.verbatim"
	// FeaturePipeCausal: pipe_send honours the argument message_id and the
	// Worker names it on the turn that answers the delivery.
	FeaturePipeCausal = "pipe.causal"
)

// Limits of an announced feature list. The hello is at most 64 KiB already;
// these keep the stored list small.
const (
	MaxFeatures   = 32
	MaxFeatureLen = 48
)

var featureName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)

// NormaliseFeatures returns the sorted, de-duplicated list of well-formed
// feature names, at most MaxFeatures. Anything else is dropped: a Worker cannot
// claim a feature by sending something odd. A nil or empty list stays nil.
func NormaliseFeatures(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range in {
		if len(f) == 0 || len(f) > MaxFeatureLen || !featureName.MatchString(f) || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	if len(out) > MaxFeatures {
		out = out[:MaxFeatures]
	}
	return out
}

// HasFeature reports whether the sorted list of features names f.
func HasFeature(sorted []string, f string) bool {
	i := sort.SearchStrings(sorted, f)
	return i < len(sorted) && sorted[i] == f
}
