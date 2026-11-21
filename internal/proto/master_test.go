package proto

import (
	"strings"
	"testing"
	"time"
)

func TestMasterWireExamples(t *testing.T) {
	open := `{"type":"master.open","seq":1,"candidates":[{"session_name":"project work","incarnation_id":"local-8c2","adapter":"claude-code-linux-tmux","catalog_result":"empty"}]}`
	var report MasterReport
	if err := DecodeMasterJSON([]byte(open), &report); err != nil || report.Validate() != nil {
		t.Fatalf("open snapshot: %+v, %v", report, err)
	}
	empty := `{"type":"master.report","seq":2,"candidates":[]}`
	if err := DecodeMasterJSON([]byte(empty), &report); err != nil || report.Validate() != nil || len(report.Candidates) != 0 {
		t.Fatalf("empty replacement was not explicit: %+v, %v", report, err)
	}

	state := `{"type":"master.state","serial":3,"report_seq":2,"ttl_ms":30000,"active":[{"incarnation_id":"local-8c2","generation":2,"lease_id":"lease-f17"}]}`
	var replacement MasterState
	if err := DecodeMasterJSON([]byte(state), &replacement); err != nil || replacement.Validate() != nil || replacement.TTLMS != (30*time.Second).Milliseconds() {
		t.Fatalf("replacement state did not preserve the lease: %v", err)
	}
	emptyState := `{"type":"master.state","serial":4,"report_seq":2,"ttl_ms":30000,"active":[]}`
	if err := DecodeMasterJSON([]byte(emptyState), &replacement); err != nil || replacement.Validate() != nil || len(replacement.Active) != 0 {
		t.Fatalf("empty binding replacement: %v", err)
	}

	call := `{"type":"op.request","op":"master_call_v1","request_id":"019aef33-01cf-7908-86fb-a82b2ebc6b91","args":{"incarnation_id":"local-8c2","generation":2,"lease_id":"lease-f17","operation":"sessions_list","arguments":{"worker_id":"019aef33-01cf-7908-86fb-a82b2ebc6b92"}}}`
	var request MasterCallRequest
	if err := DecodeMasterJSON([]byte(call), &request); err != nil || request.Validate() != nil {
		t.Fatalf("private call envelope failed: %v", err)
	}
	for _, invalid := range []string{
		strings.Replace(call, `"op":`, `"api_key":null,"op":`, 1),
		strings.Replace(call, `"op":`, `"source_session":"","op":`, 1),
		strings.Replace(call, `"worker_id":`, `"worker_id":"ignored","worker_id":`, 1),
		call + "{}",
	} {
		if err := DecodeMasterJSON([]byte(invalid), &MasterCallRequest{}); err == nil {
			t.Fatal("accepted ambiguous or unscoped master call")
		}
	}
	request.Args.Operation = "send_keys"
	if err := request.Validate(); err == nil {
		t.Fatal("accepted an operation outside the master catalog")
	}
}

func TestMasterReportRejectsIncompleteOrDuplicateCandidate(t *testing.T) {
	c := MasterCandidate{SessionName: "one", IncarnationID: "candidate-one", Adapter: MasterAdapterClaude}
	for _, cases := range [][]MasterCandidate{nil, {c, c}, {c, {SessionName: "another", IncarnationID: c.IncarnationID, Adapter: MasterAdapterClaude}}} {
		if err := (MasterReport{Type: TypeMasterOpen, Seq: 1, Candidates: cases}).Validate(); err == nil {
			t.Fatal("accepted an incomplete or ambiguous report")
		}
	}
}
