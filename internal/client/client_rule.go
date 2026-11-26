package client

import (
	"context"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

type clientRuleKey struct{}

// WithClientRule returns a context whose calls carry id as their client rule
// (proto.ClientRuleHeader): the id of the standing allow rule under which the
// call is sent, for the audit trail of the Hub. It is a label, never a
// credential. An id that is not a UUIDv7 carries none.
func WithClientRule(ctx context.Context, id string) context.Context {
	if !proto.ValidClientRule(id) {
		return ctx
	}
	return context.WithValue(ctx, clientRuleKey{}, id)
}

// ClientRule returns the client rule of [WithClientRule], or "".
func ClientRule(ctx context.Context) string {
	id, _ := ctx.Value(clientRuleKey{}).(string)
	return id
}
