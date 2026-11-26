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
	// FeatureSessionForward: the local Worker daemon forwards a session-control
	// request whose target names another Worker to its Hub (SC-5) instead of
	// running it on its own terminals. It is a property of the local control
	// socket only; the Worker does not announce it to the Hub.
	FeatureSessionForward = "session.forward"
	// FeatureSelfUpdate: the Worker serves worker_update and can replace its
	// own binary with a signed release and restart (v0.9.0). It is announced
	// only when the install method allows it (not a package, not a container,
	// not disabled with remote_update=off).
	FeatureSelfUpdate = "self.update"
	// FeaturePairAdmit: in pair_code mode the Worker accepts a pair.admit frame
	// in place of the Hub's pake.msg (a pairing request that an administrator
	// admitted after comparing the fingerprint words, v0.12.0). The Hub lists
	// only such Workers under Pairing requests.
	FeaturePairAdmit = "pair.admit"
	// FeatureMasterSessionV1 offers the session-bound master control lane.
	FeatureMasterSessionV1 = "master.session.v1"
	// FeatureSendTextAgentOnly protects the resolved pane under the local lock.
	FeatureSendTextAgentOnly = "send_text.agent_only"
	// FeatureUsageReadbackV1: the Worker serves the Hub's private read-back
	// control frame (TypeReadbackApply, see ReadbackApply): it publishes the
	// verified nominated usage document in its local presence directory and
	// acknowledges the revision. It is a control frame, not an op: the Worker
	// announces no op for it and nothing but the Hub's own authenticated
	// connection can reach it. Announced only when the Worker can publish.
	FeatureUsageReadbackV1 = "usage_readback_v1"
	// FeatureSessionInstanceV1 is reserved for the whole instance contract:
	// instance ids in sessions_list, instance-aware session events, and the
	// refusal of a target that names another instance than the session behind
	// its name. The events and the refusal are not part of the Worker yet, so
	// no Worker announces it (a client relying on the refusal would otherwise
	// reach a replacement session under the same name) and no Hub code may
	// rely on it. The listing alone is [FeatureSessionInstanceIDListV1].
	FeatureSessionInstanceV1 = "session.instance.v1"
	// FeatureSessionInstanceIDListV1: the Worker's terminal host attests an
	// instance id for the sessions it reports, and sessions_list carries it as
	// instance_id on a session row: the identity of the terminal session behind
	// a name, which survives a rename and a Worker restart and is new when the
	// name is created again. With the feature announced, a row without
	// instance_id means "the host could not tell for this session" and must be
	// treated as unverifiable; without it, it means "this Worker predates
	// instance ids or its host computes none". It is metadata only: the Worker
	// still resolves a target by its name and refuses nothing for an instance.
	// Announced only for a host that computes the ids.
	FeatureSessionInstanceIDListV1 = "session.instance_id.list.v1"
	// FeatureFilesV1: the Worker serves the file transfer ops (file_list,
	// file_pull, file_push) and the data legs of stream kind "fx" (HTTP/1.1
	// GET or PUT of FilesLegPath on one stream per leg, see files.go). The Hub
	// opens a data leg only to a Worker that announced it, and a client sends a
	// file op only to such a Worker (ops.RequiredFeatures). A Worker announces
	// it only when its host proved a root source, or with no transfer end at
	// all (Windows in v1 answers every file op with files_root_unknown).
	FeatureFilesV1 = "files.v1"
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
