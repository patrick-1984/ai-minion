package ops_test

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func workerOriginWith(f func(*proto.Origin)) proto.Origin {
	o := proto.Origin{Kind: proto.OriginWorker, SourceWorkerID: "w1", SourceWorkerFP: "fp1", SourceWorkerOwnerID: "u1"}
	f(&o)
	return o
}

// proto cannot import ops (AD-1), so this parity test lives in ops. The origin has its own session-name and UUIDv7
// rules. They must accept exactly what ops accepts: the Hub fills the origin
// from names and ids that passed the ops rules.
func TestOriginSessionNameMatchesOps(t *testing.T) {
	names := []string{
		"", "a", "planner", "my planner", "a  b", " a", "a ", "a:b", "a.b", ".", "a\tb", "a\nb", "a\x00b", "a\x7fb",
		"a\u0085b", "a\u00a0b", "pl\u00e4ne", "\u65e5\u672c", "a\xffb", "a/b", "a-b_c", "a\u200bb", "\u00a0a",
		strings.Repeat("s", 128), strings.Repeat("s", 129), strings.Repeat("\u00e4", 64), strings.Repeat("\u00e4", 65),
	}
	for _, name := range names {
		o := workerOriginWith(func(o *proto.Origin) { o.SourceSession, o.SourceProof = name, proto.OriginProofClaimed })
		// An empty name with a proof is refused for another reason; ops refuses
		// the empty name as well.
		if got, want := o.Validate() == nil, ops.ValidSessionName(name); got != want {
			t.Errorf("session name %q: origin accepts = %v, ops accepts = %v", name, got, want)
		}
	}
}

func TestOriginGrantIDMatchesOps(t *testing.T) {
	ids := []string{
		ops.NewUUIDv7(), ops.NewUUIDv7(),
		"01a00000-0000-7000-8000-0000000000aa", "01a00000-0000-7000-9000-0000000000aa",
		"01a00000-0000-7000-a000-0000000000aa", "01a00000-0000-7000-b000-0000000000aa",
		"01a00000-0000-7000-c000-0000000000aa", "01a00000-0000-7000-7000-0000000000aa",
		"01a00000-0000-4000-8000-0000000000aa", "01a00000-0000-8000-8000-0000000000aa",
		"01A00000-0000-7000-8000-0000000000AA", "01a0000000007000800000000000000000aa",
		"01a00000-0000-7000-8000-0000000000a", "01a00000-0000-7000-8000-0000000000aaa",
		"01a00000_0000-7000-8000-0000000000aa", "01a00000-0000-7000-8000-0000000000ag",
		"{1a00000-0000-7000-8000-0000000000a}", "grant-1", "x",
	}
	for _, id := range ids {
		o := workerOriginWith(func(o *proto.Origin) { o.Grant, o.GrantID = true, id })
		if got, want := o.Validate() == nil, ops.ValidUUIDv7(id); got != want {
			t.Errorf("grant id %q: origin accepts = %v, ops accepts = %v", id, got, want)
		}
	}
}
