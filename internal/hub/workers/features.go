package workers

import (
	"encoding/json"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// requiredFeatures lists the Worker features a request relies on (spine AM-13);
// the rule is ops.RequiredFeatures, shared with the local clients.
func requiredFeatures(op string, args json.RawMessage) []string {
	return ops.RequiredFeatures(op, args)
}
