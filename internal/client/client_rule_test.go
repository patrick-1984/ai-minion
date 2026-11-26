package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func TestWithClientRuleKeepsOnlyValidIDs(t *testing.T) {
	id := ops.NewUUIDv7()
	if got := ClientRule(WithClientRule(context.Background(), id)); got != id {
		t.Fatalf("got %q", got)
	}
	if got := ClientRule(WithClientRule(context.Background(), "x")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestHubSendsTheClientRuleHeaderOnlyWhenSet(t *testing.T) {
	var got []string
	f := newHubFixture(t, func(_ *hubFixture, w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(proto.ClientRuleHeader))
		jsonReply(w, 200, `{"sessions":[]}`)
	})
	b, err := NewHub(f.cfg())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	id := ops.NewUUIDv7()
	if _, err := b.Call(WithClientRule(context.Background(), id), ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != id || got[1] != "" {
		t.Fatalf("headers = %q", got)
	}
}
