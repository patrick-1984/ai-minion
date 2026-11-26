package ops

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	fxWorker = "01890a5d-ac96-774b-bcce-b302099a8057"
	fxID     = "01890a5d-ac96-774b-8cce-b302099a8058"
)

var fxToken = strings.Repeat("A", proto.LegTokenLen)

// The five file ops are declared with the contract of BRD FR-1: none streams,
// none is a Write, the admissions and the receipt Mutate, every one is strict
// and Unreleased.
func TestFileOpsCatalogTable(t *testing.T) {
	want := "" +
		"file_list | worker | actions file_read | write=false stream=false | args FileListArgs | result FileListResult | guards -\n" +
		"file_pull | worker | actions file_read | write=false stream=false | args FilePullArgs | result FileTransferResult | guards -\n" +
		"file_push | worker | actions file_write | write=false stream=false | args FilePushArgs | result FileTransferResult | guards -\n" +
		"file_receipt | hub | actions file_read|file_write | write=false stream=false | args FileReceiptArgs | result FileReceiptResult | guards -\n" +
		"file_transfers_list | hub | actions file_read | write=false stream=false | args FileTransfersListArgs | result FileTransfersListResult | guards -\n"
	if got := CatalogTable(Select(FileOpNames...)); got != want {
		t.Errorf("catalog table\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	mutates := map[string]bool{OpFilePull: true, OpFilePush: true, OpFileReceipt: true}
	for _, op := range Select(FileOpNames...) {
		if op.Mutates != mutates[op.Name] || op.Write || op.Stream || op.SessionOnly {
			t.Errorf("%s: mutates=%v write=%v stream=%v", op.Name, op.Mutates, op.Write, op.Stream)
		}
		if !op.Unreleased {
			t.Errorf("%s is not Unreleased", op.Name)
		}
		if !StrictArgs(op.Name) {
			t.Errorf("%s is not strict", op.Name)
		}
		if IsFileOp(op.Name) != (op.Scope == ScopeWorker) {
			t.Errorf("IsFileOp(%s) = %v", op.Name, IsFileOp(op.Name))
		}
	}
	// No other op is Unreleased.
	for _, op := range All() {
		if op.Unreleased && !slices.Contains(FileOpNames, op.Name) {
			t.Errorf("%s is Unreleased", op.Name)
		}
	}
}

func TestFileActions(t *testing.T) {
	for _, a := range []Action{ActionFileRead, ActionFileWrite} {
		if !a.Valid() {
			t.Errorf("%s is not valid", a)
		}
	}
	if ActionFileRead != "file_read" || ActionFileWrite != "file_write" {
		t.Fatal("the action names are wire format")
	}
	set, err := ParseActionSet("file_write|file_read")
	if err != nil || set.String() != "file_read|file_write" {
		t.Fatalf("ParseActionSet = %v, %v", set, err)
	}
	// Directional, and implied by nothing.
	if MustActionSet(ActionFileRead).Permits(ActionRead, ActionOperate, ActionManage, ActionFileWrite) {
		t.Error("file_read is implied by another action")
	}
	if MustActionSet(ActionFileWrite).Permits(ActionRead, ActionOperate, ActionManage, ActionFileRead) {
		t.Error("file_write is implied by another action")
	}
}

func TestFileOpsRequireFilesFeature(t *testing.T) {
	for _, op := range []string{OpFileList, OpFilePull, OpFilePush} {
		for _, args := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`not json`)} {
			if got := RequiredFeatures(op, args); !slices.Equal(got, []string{proto.FeatureFilesV1}) {
				t.Errorf("RequiredFeatures(%s) = %v", op, got)
			}
			if got := LocalRequiredFeatures(op, args); !slices.Contains(got, proto.FeatureFilesV1) {
				t.Errorf("LocalRequiredFeatures(%s) = %v", op, got)
			}
		}
	}
	for _, op := range []string{OpFileReceipt, OpFileTransfersList} {
		if got := RequiredFeatures(op, nil); got != nil {
			t.Errorf("RequiredFeatures(%s) = %v", op, got)
		}
	}
}

func TestFileHiddenMembers(t *testing.T) {
	for _, m := range []string{"transfer_id", "leg_token", "max_bytes", "max_entries"} {
		if !HiddenMember(OpFilePull, m) || !HiddenMember(OpFilePush, m) {
			t.Errorf("%s is not hidden", m)
		}
	}
	for _, m := range []string{"kind", "size", "entries"} {
		if !HiddenMember(OpFilePush, m) {
			t.Errorf("file_push.%s is not hidden", m)
		}
	}
	for _, m := range []string{"target", "path", "to", "overwrite"} {
		if HiddenMember(OpFilePull, m) {
			t.Errorf("file_pull.%s is hidden", m)
		}
	}
	if HiddenMember(OpFilePush, "as") || HiddenMember(OpFileReceipt, "leg_token") {
		t.Error("a caller member is hidden")
	}
}

func TestCheckFilePath(t *testing.T) {
	for _, p := range []string{"a", "a.txt", "dir/sub/file", ".git/HEAD", "%2e%2e/x", "Makefile", "a b/c",
		strings.Repeat("n", proto.MaxNameBytes), strings.TrimSuffix(strings.Repeat("d/", proto.MaxPathDepth), "/"),
		".minion/other", "x/" + proto.ManifestName} {
		if err := CheckFilePath(p, false); err != nil {
			t.Errorf("CheckFilePath(%q) = %v", p, err)
		}
	}
	hostile := []string{"", ".", "..", "../x", "/etc/passwd", "a/../../b", `C:\x`, `a\..\b`, "a/./b", "a//b", "a/",
		"a\x00b", "a\nb", "a\x7fb", "a\u0085b", "\xff", strings.Repeat("n", proto.MaxNameBytes+1),
		strings.Repeat("d/", proto.MaxPathDepth) + "x", strings.Repeat("a", proto.MaxPathBytes+1),
		proto.StagingPrefix + "x", "dir/" + proto.StagingPrefix + "1-2", proto.ManifestName}
	for _, p := range hostile {
		err := CheckFilePath(p, false)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.CodePathOutsideRoot {
			t.Errorf("CheckFilePath(%q) = %v, want path_outside_root", p, err)
			continue
		}
		if len(p) > 2 && strings.Contains(pe.Message, p) {
			t.Errorf("the refusal of %q repeats the path", p)
		}
	}
	if err := CheckFilePath("", true); err != nil {
		t.Errorf("the root is refused: %v", err)
	}
}

func TestFileArgsValidate(t *testing.T) {
	tg := Target{WorkerID: fxWorker, SessionName: "work"}
	ok := []Validator{
		FileListArgs{Target: tg},
		FileListArgs{Target: tg, Path: "a", Cursor: "abc-_9", Limit: proto.FileListMaxLimit},
		FilePullArgs{Target: tg, Path: "a"},
		FilePullArgs{Target: tg, Path: "a", FileLegArgs: FileLegArgs{TransferID: fxID, LegToken: fxToken, MaxBytes: 1}},
		FilePushArgs{Target: tg, Path: "a", As: "b", Kind: FileKindDir, Size: 10, Entries: 2},
		FileTransfersListArgs{},
		FileTransfersListArgs{WorkerID: fxWorker, TransferID: fxID, Limit: FileTransfersMaxLimit},
		FileReceiptArgs{TransferID: fxID, LegToken: fxToken, FileReceipt: FileReceipt{Outcome: OutcomeDelivered, ManifestDigest: strings.Repeat("a", 64)}},
		FileReceiptArgs{TransferID: fxID, LegToken: fxToken, FileReceipt: FileReceipt{Outcome: OutcomeFailedNotSent, Reason: string(proto.CodeIntegrityFailed)}},
	}
	for i, a := range ok {
		if err := a.Validate(); err != nil {
			t.Errorf("ok[%d] %T: %v", i, a, err)
		}
	}
	bad := map[string]Validator{
		"session_name": FileListArgs{},
		"cursor":       FileListArgs{Target: tg, Cursor: "a b"},
		"limit":        FileListArgs{Target: tg, Limit: proto.FileListMaxLimit + 1},
		"leg_token":    FilePullArgs{Target: tg, FileLegArgs: FileLegArgs{TransferID: fxID}},
		"transfer_id":  FilePullArgs{Target: tg, FileLegArgs: FileLegArgs{TransferID: "x", LegToken: fxToken}},
		"max_bytes":    FilePushArgs{Target: tg, FileLegArgs: FileLegArgs{MaxBytes: -1}},
		"kind":         FilePushArgs{Target: tg, Kind: "symlink"},
		"size":         FilePushArgs{Target: tg, Size: -1},
		"worker_id":    FileTransfersListArgs{WorkerID: "x"},
		"outcome":      FileReceiptArgs{TransferID: fxID, LegToken: fxToken},
		"reason":       FileReceiptArgs{TransferID: fxID, LegToken: fxToken, FileReceipt: FileReceipt{Outcome: OutcomePartial, Reason: "nope"}},
		"manifest_digest": FileReceiptArgs{TransferID: fxID, LegToken: fxToken,
			FileReceipt: FileReceipt{Outcome: OutcomeDelivered}},
		"entries": FileReceiptArgs{TransferID: fxID, LegToken: fxToken,
			FileReceipt: FileReceipt{Outcome: OutcomeUnknown, Entries: make([]FileTransferEntry, proto.MaxResultEntries+1)}},
	}
	for field, a := range bad {
		err := a.Validate()
		var ae *ArgumentError
		if !errors.As(err, &ae) || ae.Field != field {
			t.Errorf("%s: %T = %v, want invalid %s", field, a, err, field)
		}
	}
	if (FileListArgs{}).EffectiveLimit() != proto.FileListDefaultLimit || (FileTransfersListArgs{}).EffectiveLimit() != FileTransfersDefaultLimit {
		t.Error("default limits")
	}
}

// Every result type of the file ops is a JSON object with arrays, never null.
func TestFileResultsEncode(t *testing.T) {
	for _, v := range []any{FileListResult{}, FileTransfersListResult{}} {
		b, err := json.Marshal(v)
		if err != nil || strings.Contains(string(b), "null") {
			t.Errorf("%T = %s, %v", v, b, err)
		}
	}
	// The receipt members are flat in file_receipt.
	b, _ := json.Marshal(FileReceiptArgs{TransferID: fxID, LegToken: fxToken, FileReceipt: FileReceipt{Outcome: OutcomeDelivered}})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["outcome"] != "delivered" || m["transfer_id"] != fxID {
		t.Errorf("file_receipt = %s", b)
	}
	// The hidden members are flat in file_pull too.
	b, _ = json.Marshal(FilePullArgs{FileLegArgs: FileLegArgs{TransferID: fxID}})
	if !strings.Contains(string(b), `"transfer_id":"`+fxID+`"`) {
		t.Errorf("file_pull = %s", b)
	}
}

func TestFileModesAndEffects(t *testing.T) {
	tgt, caller, ok := FileEffects(FileDirectionPull)
	if !ok || tgt != FileEffectSend || caller != FileEffectReceive {
		t.Error("pull effects")
	}
	tgt, caller, ok = FileEffects(FileDirectionPush)
	if !ok || tgt != FileEffectReceive || caller != FileEffectSend {
		t.Error("push effects")
	}
	if _, _, ok := FileEffects("sync"); ok {
		t.Error("unknown direction")
	}
	allow := map[string][2]bool{ // send, receive
		FileModeOn: {true, true}, FileModeSendOnly: {true, false}, FileModeReceiveOnly: {false, true},
		FileModeOff: {false, false}, "": {false, false}, "bogus": {false, false},
	}
	for mode, want := range allow {
		if WorkerFileModeAllows(mode, FileEffectSend) != want[0] || WorkerFileModeAllows(mode, FileEffectReceive) != want[1] {
			t.Errorf("WorkerFileModeAllows(%q)", mode)
		}
		if ValidWorkerFileMode(mode) != (mode != "" && mode != "bogus") {
			t.Errorf("ValidWorkerFileMode(%q)", mode)
		}
	}
	if WorkerFileModeAllows(FileModeOn, "other") {
		t.Error("unknown effect allowed")
	}
	type c struct {
		policy, dir string
		admin, want bool
	}
	for _, tc := range []c{
		{FileModeOn, FileDirectionPull, false, true}, {FileModeOn, FileDirectionPush, false, true},
		{FilePolicyPullOnly, FileDirectionPull, false, true}, {FilePolicyPullOnly, FileDirectionPush, true, false},
		{FilePolicyPushOnly, FileDirectionPush, false, true}, {FilePolicyPushOnly, FileDirectionPull, true, false},
		{FilePolicyAdminsOnly, FileDirectionPull, false, false}, {FilePolicyAdminsOnly, FileDirectionPush, true, true},
		{FileModeOff, FileDirectionPull, true, false}, {"", FileDirectionPull, true, false}, {FileModeOn, "x", true, false},
	} {
		if got := HubFilePolicyAllows(tc.policy, tc.dir, tc.admin); got != tc.want {
			t.Errorf("HubFilePolicyAllows(%q, %q, %v) = %v", tc.policy, tc.dir, tc.admin, got)
		}
	}
	for _, p := range []string{FileModeOn, FilePolicyPullOnly, FilePolicyPushOnly, FileModeOff, FilePolicyAdminsOnly} {
		if !ValidHubFilePolicy(p) {
			t.Errorf("ValidHubFilePolicy(%q)", p)
		}
	}
	if ValidHubFilePolicy(FileModeSendOnly) {
		t.Error("send_only is not a Hub-wide policy")
	}
	for _, s := range []string{FileStateAdmitted, FileStatePrepared, FileStateDelivered, FileStatePartial,
		FileStateFailedNotSent, FileStateServed, FileStateUnknown, FileStateDenied} {
		if !ValidFileState(s) {
			t.Errorf("ValidFileState(%q)", s)
		}
	}
}

func TestFileTransferLimitsLower(t *testing.T) {
	d := DefaultFileTransferLimits()
	if d.MaxBytes != 4<<30 || d.MaxEntries != 100000 || d.MaxDepth != 32 || d.MaxPathBytes != 4096 {
		t.Fatalf("defaults = %+v", d)
	}
	got := d.Lower(FileTransferLimits{MaxBytes: 1 << 20, MaxEntries: 1 << 30})
	if got.MaxBytes != 1<<20 || got.MaxEntries != d.MaxEntries || got.MaxDepth != d.MaxDepth {
		t.Errorf("Lower = %+v", got)
	}
}

// The grant and acceptance members of the file flags: absent on old rows,
// files_push only at level control, flags only with accept true.
func TestFileGrantAndAcceptanceFlags(t *testing.T) {
	yes := true
	base := GrantSetArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, SourceWorkerID: fxWorker,
		TargetWorkerID: "01890a5d-ac96-774b-bcce-b302099a8059", SourceSession: "m", Level: GrantLevelView}
	a := base
	a.FilesPull = &yes
	if err := a.Validate(); err != nil {
		t.Errorf("view + files_pull: %v", err)
	}
	a.FilesPush = &yes
	var ae *ArgumentError
	if err := a.Validate(); !errors.As(err, &ae) || ae.Field != "files_push" {
		t.Errorf("view + files_push = %v", err)
	}
	a.Level = GrantLevelControl
	if err := a.Validate(); err != nil {
		t.Errorf("control + files_push: %v", err)
	}
	var g Grant
	if err := json.Unmarshal([]byte(`{"id":"x","level":"control"}`), &g); err != nil || g.FilesPull || g.FilesPush {
		t.Errorf("an old grant reads %+v, %v", g, err)
	}
	acc := ControlSetAcceptArgs{WriteArgs: WriteArgs{RequestID: NewUUIDv7()}, WorkerID: fxWorker, SourceWorkerID: fxWorker, FilesPull: &yes}
	if err := acc.Validate(); !errors.As(err, &ae) || ae.Field != "files_pull" {
		t.Errorf("accept false + files_pull = %v", err)
	}
	acc.Accept = true
	if err := acc.Validate(); err != nil {
		t.Errorf("accept + files_pull: %v", err)
	}
	if err := (ControlSetAcceptArgs{}).Validate(); !errors.As(err, &ae) || ae.Field != "request_id" {
		t.Errorf("the request id is still checked: %v", err)
	}
}

func TestFileOpsInOpenAPI(t *testing.T) {
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Paths      map[string]map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	get := map[string]bool{OpFileList: true, OpFileTransfersList: true}
	for _, name := range FileOpNames {
		op, _ := Lookup(name)
		item, ok := d.Paths[op.RESTPath()]
		if !ok {
			t.Errorf("%s is missing", name)
			continue
		}
		if _, has := item["get"]; has != get[name] {
			t.Errorf("%s: GET = %v", name, has)
		}
		if item["post"]["x-minion-unreleased"] != true {
			t.Errorf("%s is not marked unreleased", name)
		}
		for _, ty := range []reflect.Type{op.Args, op.Result} {
			if _, ok := d.Components.Schemas[ty.Name()]; !ok {
				t.Errorf("%s: schema %s is missing", name, ty.Name())
			}
		}
	}
	if _, has := d.Paths[RESTPrefix+OpSessionsList]["post"]["x-minion-unreleased"]; has {
		t.Error("a released op is marked unreleased")
	}
}
