package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The attribution members of an origin (story W1): source_session,
// source_proof and grant_id. The Worker keeps them in its local audit and
// decides nothing by them.

const originGrantID = "01a00000-0000-7000-8000-0000000000aa"

// attributed returns o with a calling session, its proof and, when the origin
// is marked with a grant, the id of that grant.
func attributed(o proto.Origin, session, proof string) proto.Origin {
	o.SourceSession, o.SourceProof = session, proof
	if o.Grant {
		o.GrantID = originGrantID
	}
	return o
}

func TestAuditOriginKeepsTheAttributionMembers(t *testing.T) {
	o := workerOrigin(otherWorker, "fp-other", ownerB)
	o.Grant = true
	*o = attributed(*o, "my planner", proto.OriginProofClaimed)
	text := auditOrigin(context.WithValue(context.Background(), originCtxKey{}, o))
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("audit origin %q: %v", text, err)
	}
	if m["source_session"] != "my planner" || m["source_proof"] != "claimed" || m["grant_id"] != originGrantID || m["grant"] != true {
		t.Fatalf("audit origin = %s", text)
	}
	if got := auditOrigin(context.Background()); got != "local" {
		t.Fatalf("audit origin of a local call = %q", got)
	}
}

// A Hub write whose origin names the calling session and the grant is audited
// with them, and so is a write refused for its origin.
func TestWorkerAuditRecordsTheAttributionMembersOfHubWrites(t *testing.T) {
	o := newOriginEnv(t, "none")
	granted := workerOrigin(otherWorker, "fp-other", ownerB)
	granted.Grant = true
	*granted = attributed(*granted, "my planner", proto.OriginProofClaimed)
	if typ, code := o.serve(ops.OpSendText, messageArgs(ops.OpSendText), granted); typ != proto.TypeOpResult {
		t.Fatalf("%s %s", typ, code)
	}
	refused := workerOrigin(otherWorker, "fp-other", ownerB)
	*refused = attributed(*refused, "planner", proto.OriginProofVerified)
	if typ, code := o.serve(ops.OpSendText, messageArgs(ops.OpSendText), refused); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
		t.Fatalf("without a grant: %s %s, want pipe_origin_refused", typ, code)
	}
	rows := o.audit()
	if len(rows) != 2 {
		t.Fatalf("audit = %+v", rows)
	}
	seen := map[proto.Origin]bool{}
	for _, row := range rows {
		var got proto.Origin
		if err := json.Unmarshal([]byte(row.Origin), &got); err != nil {
			t.Fatalf("audit origin %q: %v", row.Origin, err)
		}
		seen[got] = true
	}
	if !seen[*granted] || !seen[*refused] {
		t.Fatalf("audit origins = %+v\nwant %+v\nand  %+v", rows, *granted, *refused)
	}
	for _, row := range rows {
		if strings.Contains(row.Origin, `"source_session":"my planner"`) && !strings.Contains(row.Origin, `"grant_id":"`+originGrantID+`"`) {
			t.Fatalf("the granted row lacks the grant id: %s", row.Origin)
		}
	}
}

// The attribution members never change what Policy.CheckOrigin decides: for
// every policy and every source, the origin with them is judged exactly as the
// origin without them.
func TestCheckOriginIgnoresTheAttributionMembers(t *testing.T) {
	fpOther := strings.Repeat("a", 52)
	bases := map[string]proto.Origin{
		"own Worker":            *workerOrigin(selfWorker, "fp-self", ownerA),
		"Worker of the owner":   *workerOrigin(peerWorker, "fp-peer", ownerA),
		"Worker of another":     *workerOrigin(otherWorker, fpOther, ownerB),
		"granted, of the owner": {Kind: proto.OriginWorker, SourceWorkerID: peerWorker, SourceWorkerFP: "fp-peer", SourceWorkerOwnerID: ownerA, Grant: true},
		"granted, of another":   {Kind: proto.OriginWorker, SourceWorkerID: otherWorker, SourceWorkerFP: fpOther, SourceWorkerOwnerID: ownerB, Grant: true},
	}
	variants := map[string]func(proto.Origin) proto.Origin{
		"claimed session":  func(o proto.Origin) proto.Origin { return attributed(o, "planner", proto.OriginProofClaimed) },
		"verified session": func(o proto.Origin) proto.Origin { return attributed(o, "planner", proto.OriginProofVerified) },
		"session named like the accepted Worker": func(o proto.Origin) proto.Origin {
			return attributed(o, peerWorker, proto.OriginProofVerified)
		},
	}
	for _, accept := range []string{"none", "owner-workers", peerWorker, fpOther} {
		a, err := config.ParsePipeAcceptFrom(accept)
		if err != nil {
			t.Fatal(err)
		}
		p := NewPolicy(nil)
		p.SetPipeAcceptFrom(a)
		p.SetWorkerID(selfWorker)
		p.SetOwnerID(ownerA)
		for bname, base := range bases {
			want := p.CheckOrigin(base)
			for vname, variant := range variants {
				o := variant(base)
				if err := o.Validate(); err != nil {
					t.Fatalf("%s, %s: %v", bname, vname, err)
				}
				got := p.CheckOrigin(o)
				if (got == nil) != (want == nil) || proto.CodeOf(got) != proto.CodeOf(want) {
					t.Errorf("pipe_accept_from=%s, %s, %s: CheckOrigin = %v, without the members %v", accept, bname, vname, got, want)
				}
			}
		}
	}
	// A nil policy accepts no other Worker, whatever the origin claims.
	var none *Policy
	if err := none.CheckOrigin(attributed(bases["Worker of another"], "planner", proto.OriginProofVerified)); err == nil {
		t.Fatal("a nil policy accepted another Worker for its attribution members")
	}
}

// Through the op server: a verified calling session and a grant id do not make
// a message acceptable. Only the grant mark and pipe_accept_from decide.
func TestAttributionMembersDoNotBypassPipeAcceptFrom(t *testing.T) {
	o := newOriginEnv(t, "none")
	plain := workerOrigin(otherWorker, "fp-other", ownerB)
	*plain = attributed(*plain, "planner", proto.OriginProofVerified)
	for _, op := range messageOps {
		if typ, code := o.serve(op, messageArgs(op), plain); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
			t.Fatalf("%s with a verified session and no grant: %s %s, want pipe_origin_refused", op, typ, code)
		}
		// A grant id without the grant mark is malformed, never a grant.
		forged := map[string]any{"kind": "worker", "source_worker_id": otherWorker, "source_worker_fp": "fp-other",
			"source_worker_owner_id": ownerB, "grant_id": originGrantID}
		if typ, code := o.serve(op, messageArgs(op), forged); typ != proto.TypeError || code != proto.CodePipeOriginRefused {
			t.Fatalf("%s with a grant id and no grant: %s %s, want pipe_origin_refused", op, typ, code)
		}
	}
	if n := len(o.host.pastes()); n != 0 {
		t.Fatalf("%d pastes for refused origins", n)
	}
}

// The Worker decodes the origin without refusing members it does not know.
// That is why a Worker of the release before the attribution members accepts
// an origin that carries them, and why this Worker accepts members of a later
// Hub.
func TestOriginWithUnknownMembersIsAccepted(t *testing.T) {
	o := newOriginEnv(t, "owner-workers")
	origin := map[string]any{"kind": "worker", "source_worker_id": peerWorker, "source_worker_fp": "fp-peer",
		"source_worker_owner_id": ownerA, "a_member_of_a_later_hub": "x", "another": map[string]any{"n": 1}}
	if typ, code := o.serve(ops.OpSendText, messageArgs(ops.OpSendText), origin); typ != proto.TypeOpResult {
		t.Fatalf("an origin with unknown members: %s %s", typ, code)
	}
	rows := o.audit()
	if len(rows) != 1 || strings.Contains(rows[0].Origin, "a_member_of_a_later_hub") {
		t.Fatalf("audit = %+v", rows)
	}
}

// No Worker code reads the attribution members: they are written to the audit
// by marshalling the whole origin. A selector on one of them in non-test code
// of the Worker fails here; if it is a decision, it must not be added. The
// frame of a local call (call.go) has its own SourceSession member, which is
// not the origin's.
func TestNoWorkerCodeReadsTheAttributionMembers(t *testing.T) {
	read := regexp.MustCompile(`\.(SourceSession|SourceProof|GrantID)\b`)
	other := map[string]string{filepath.Join("..", "agent", "call.go"): "SourceSession"}
	files := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range read.FindAllStringSubmatch(line, -1) {
				if other[path] == m[1] {
					continue
				}
				t.Errorf("%s:%d reads %s, an attribution member of the origin: %s", path, i+1, m[1], strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("only %d Go files of the Worker were read", files)
	}
}
