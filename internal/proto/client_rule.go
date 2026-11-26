package proto

// The client rule of a request: the id of the standing allow rule ("minion
// claude allow") under which a client such as "minion mcp" sent a write
// without asking its user.
//
// The id is a label for the audit trail only. The client chose it and no layer
// verifies it, so it never authorizes anything: the Hub records it in the
// detail of the audit event of the write ("client_rule=<id>"), and every
// decision stays the one of authz.Decide. A value that is not a UUIDv7 is
// dropped, never refused. A Worker or Hub that predates the member ignores it.
//
// It travels as the optional client_rule member of the local op.request frame
// (client to Worker), of the call frame (Worker to Hub) and, on REST, as the
// [ClientRuleHeader] request header.

// ClientRuleHeader is the HTTP request header of the client rule on the Hub's
// REST API.
const ClientRuleHeader = "Minion-Client-Rule"

// ClientRuleDetailKey is the key of the client rule in an audit detail.
const ClientRuleDetailKey = "client_rule"

// ValidClientRule reports whether id can be carried as a client rule: a
// UUIDv7 in canonical lowercase text form.
func ValidClientRule(id string) bool { return validOriginUUIDv7(id) }

// ClientRuleDetail returns the audit detail pair of the client rule id, or ""
// when id is empty or not a valid client rule.
func ClientRuleDetail(id string) string {
	if !ValidClientRule(id) {
		return ""
	}
	return ClientRuleDetailKey + "=" + id
}

// AppendDetail joins two audit details with one space; either may be empty.
func AppendDetail(detail, pair string) string {
	switch {
	case pair == "":
		return detail
	case detail == "":
		return pair
	}
	return detail + " " + pair
}
