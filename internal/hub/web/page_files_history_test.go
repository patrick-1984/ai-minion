package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func historyRecord() ops.FileTransferRecord {
	return ops.FileTransferRecord{
		TransferID: filesTransferID, Direction: "push", State: ops.FileStateDelivered, Principal: "alice <admin>",
		TargetWorkerID: sessWorkerID, TargetSession: "minion-1", TargetRoot: "~/app", Path: `docs/<b>x</b>.md`,
		Kind: ops.FileKindFile, Files: 1, Bytes: 4096, ManifestDigest: strings.Repeat("ab", 32), CreatedAt: adminTime,
	}
}

func TestFilesHistoryListsWhatTheOperationReturns(t *testing.T) {
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{sessionWorker()}}).
		reply(ops.OpFileTransfersList, ops.FileTransfersListResult{Transfers: []ops.FileTransferRecord{historyRecord()}, NextCursor: "n_1"})
	connect(t, f)
	s := adminServer(t)
	body := get(t, s, "/files/history?cursor=c_0", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, "<h1>File transfers</h1>", "minion-1 on pi-1", "docs/&lt;b&gt;x&lt;/b&gt;.md", "alice &lt;admin&gt;",
		`data-tone="positive">delivered</span>`, "1 files, 4.0 KiB", `href="/files/history?transfer=`+filesTransferID+`"`,
		`href="/files/history?cursor=n_1"`)
	mustNotHave(t, body, "<b>x</b>")
	calls := f.called(ops.OpFileTransfersList)
	if len(calls) != 1 || calls[0].Args["cursor"] != "c_0" || calls[0].User != "bob" {
		t.Fatalf("calls = %+v", calls)
	}

	// One transfer with its entries.
	rec := historyRecord()
	rec.Entries = []ops.FileTransferEntry{{Path: "docs/x.md", Size: 4096, SHA256: strings.Repeat("cd", 32)}, {Path: "docs/.env", SkippedReason: "sensitive"}}
	rec.EntriesOmitted = 3
	f.reply(ops.OpFileTransfersList, ops.FileTransfersListResult{Transfers: []ops.FileTransferRecord{rec}})
	body = get(t, s, "/files/history?transfer="+url.QueryEscape(filesTransferID), "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, "Transfer "+filesTransferID, strings.Repeat("cd", 32), "sensitive, not transferred", "3 more entries", strings.Repeat("ab", 32))
	if c := f.called(ops.OpFileTransfersList); c[len(c)-1].Args["transfer_id"] != filesTransferID {
		t.Fatalf("args = %+v", c[len(c)-1].Args)
	}

	// A malformed id is not sent; a refusal is one line.
	n := len(f.called(ops.OpFileTransfersList))
	body = get(t, s, "/files/history?transfer=x", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, "That is not a transfer id.")
	if len(f.called(ops.OpFileTransfersList)) != n {
		t.Error("a malformed transfer id reached the Hub")
	}
	f.on(ops.OpFileTransfersList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "not allowed"}
	})
	mustHave(t, get(t, s, "/files/history", "X-Test-Session", userHeader).Body.String(), "The history could not be read: not allowed")
}

func TestGrantAddFileFlagsAreConfirmedAndSent(t *testing.T) {
	f := newFakeOps().reply(ops.OpGrantList, grantFixture()).reply(ops.OpGrantSet, ops.GrantSetResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	d := Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}}
	s, _ := listServer(t, d, d)
	add := url.Values{"source_worker_id": {"w1"}, "source_session": {"w1|fix-tests"}, "target_worker_id": {"w1"},
		"target_scope": {"one"}, "target_session": {"w1|ci"}, "level": {"view"}, "files_push": {"1"}}
	rec := post(t, s, pathAccessGrants+"/add", adminHeader, add)
	mustHave(t, rec.Body.String(), "Pushing files needs the level Control.")
	add.Del("files_push")
	add.Set("files_pull", "1")
	rec = post(t, s, pathAccessGrants+"/add", adminHeader, add)
	mustHave(t, rec.Body.String(), "It may also copy files out of the target session")
	if len(f.called(ops.OpGrantSet)) != 0 {
		t.Fatal("a grant with file flags was saved without the confirmation")
	}
	key := grantConfirmKeyForTest("w1", "fix-tests", "w1", "one", "ci", "view", "", "files:pull")
	add.Set("confirm", "1")
	add.Set("confirm_for", key)
	post(t, s, pathAccessGrants+"/add", adminHeader, add)
	calls := f.called(ops.OpGrantSet)
	if len(calls) != 1 || calls[0].Args["files_pull"] != true || calls[0].Args["files_push"] != nil {
		t.Fatalf("calls = %+v", calls)
	}
	// The dialog offers both flags unchecked.
	body := get(t, s, pathAccessGrants, "X-Test-Session", adminHeader).Body.String()
	mustHave(t, body, `<input type="checkbox" name="files_pull" id="grant-add-files-pull" value="1">`, `<input type="checkbox" name="files_push" id="grant-add-files-push" value="1">`)
}
