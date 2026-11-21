package workers

import (
	"encoding/json"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// requiredFeatures lists the Worker features a request relies on (spine AM-13);
// the rule is ops.RequiredFeatures, shared with the local clients.
func requiredFeatures(op string, args json.RawMessage) []string {
	need := ops.RequiredFeatures(op, args)
	if op == ops.OpSendText {
		var a ops.SendTextArgs
		if json.Unmarshal(args, &a) == nil && a.AgentOnly {
			need = append(need, proto.FeatureSendTextAgentOnly)
		}
	}
	return need
}
