package ops

import (
	"encoding/json"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// RequiredFeatures lists the Worker features that a request of op with args
// relies on (spine AM-13). It is the one rule, used by the Hub when it routes a
// request to a Worker and by the local clients (CLI, MCP) when they send one to
// the local Worker daemon, so that the two cannot drift.
//
// A Worker ignores every argument field it does not know (AD-4), so an older
// Worker would take a reviewed answer for a plain one, an atomic submit for a
// paste without Enter and a multi-line text for several lines of typed input.
// The request must therefore be sent only to a Worker that announced what it
// needs:
//
//   - answer: always proto.FeatureAnswerReviewed. Its expect argument is
//     required, and a Worker without the binding would send the keys to whatever
//     dialog is open.
//   - send_text: proto.FeatureSendTextSubmit when submit is set, and
//     proto.FeatureTextVerbatim unless the text is plain (one line of printable
//     text, which every Worker delivers the same way). A default send_text of a
//     plain text still reaches an older Worker.
//   - pipe_send: proto.FeaturePipeCausal (the Hub names the delivery in
//     message_id, and a Worker that ignores it emits replies the Hub cannot
//     match), and proto.FeatureTextVerbatim for a text that is not plain.
//
// Other ops, reads included, need nothing. An argument that cannot be read
// requires every feature of the op, so a malformed request never weakens a
// check.
func RequiredFeatures(op string, args json.RawMessage) []string {
	switch op {
	case OpAnswer:
		return []string{proto.FeatureAnswerReviewed}
	case OpSendText:
		var a struct {
			Text   string `json:"text"`
			Submit bool   `json:"submit"`
		}
		if json.Unmarshal(args, &a) != nil {
			return []string{proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}
		}
		var need []string
		if a.Submit {
			need = append(need, proto.FeatureSendTextSubmit)
		}
		if !PlainText(a.Text) {
			need = append(need, proto.FeatureTextVerbatim)
		}
		return need
	case OpPipeSend:
		var a struct {
			Text string `json:"text"`
		}
		need := []string{proto.FeaturePipeCausal}
		if json.Unmarshal(args, &a) != nil || !PlainText(a.Text) {
			need = append(need, proto.FeatureTextVerbatim)
		}
		return need
	}
	return nil
}

// LocalRequiredFeatures is RequiredFeatures for a request that goes to the local
// Worker daemon over its control socket. The causal id of a pipe delivery is a
// property of the Hub-to-Worker request (the Hub sets message_id), not of the
// local entry, so pipe_send needs only the verbatim paste there.
func LocalRequiredFeatures(op string, args json.RawMessage) []string {
	var out []string
	for _, f := range RequiredFeatures(op, args) {
		if f != proto.FeaturePipeCausal {
			out = append(out, f)
		}
	}
	return out
}
