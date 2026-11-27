//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// WP11: pull and push of a file and a folder through the real Hub, and a
// relay between two Workers (pulled from one, pushed to the other) by the
// REST caller end.

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestFilesPullAndPushOfAFileAndAFolderBetweenTwoWorkersThroughTheHub(t *testing.T) {
	e := startFilesE2E(t)
	wa := e.startWorker("wa", fxWorkerOptions{}, "src")
	wb := e.startWorker("wb", fxWorkerOptions{}, "dst")
	bearer := e.key("files", fileActions, wa, wb)
	c := e.caller(bearer)
	ctx := ctxT(t)

	big := pattern(3 << 20) // compressible, over several chunks
	writeFile(t, wa.dirs["src"], "report.txt", big)
	writeFile(t, wa.dirs["src"], "proj/a.txt", []byte("alpha\n"))
	writeFile(t, wa.dirs["src"], "proj/sub/b.bin", noise(200<<10))
	writeFile(t, wa.dirs["src"], "proj/sub/deeper/c.txt", []byte("charlie\n"))

	// file_list of the target session folder.
	var list ops.FileListResult
	if err := e.call(bearer, ops.OpFileList, ops.FileListArgs{Target: target(wa, "src")}, &list); err != nil {
		t.Fatalf("file_list: %v", err)
	}
	names := map[string]string{}
	for _, en := range list.Entries {
		names[en.Name] = en.Type
	}
	if names["report.txt"] != proto.FileTypeFile || names["proj"] != proto.FileTypeDir {
		t.Fatalf("file_list entries: %+v", list.Entries)
	}

	// Pull of one file.
	res, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: target(wa, "src"), Path: "report.txt"})
	if err != nil {
		t.Fatalf("pull of a file: %v", err)
	}
	if res.Outcome != ops.OutcomeDelivered || res.Bytes != int64(len(big)) {
		t.Fatalf("pull of a file: %+v", res)
	}
	if got := readFile(t, c.dir, "report.txt"); !bytes.Equal(got, big) {
		t.Fatalf("pulled file differs: %s vs %s", sum(got), sum(big))
	}

	// Pull of a folder.
	res, err = agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: target(wa, "src"), Path: "proj"})
	if err != nil {
		t.Fatalf("pull of a folder: %v", err)
	}
	if res.Outcome != ops.OutcomeDelivered || res.Files != 3 {
		t.Fatalf("pull of a folder: %+v", res)
	}
	for _, rel := range []string{"proj/a.txt", "proj/sub/b.bin", "proj/sub/deeper/c.txt"} {
		if !bytes.Equal(readFile(t, c.dir, rel), readFile(t, wa.dirs["src"], rel)) {
			t.Fatalf("%s differs after the folder pull", rel)
		}
	}

	// Push of the pulled file and folder to the other Worker: the relay
	// between two Workers through the Hub.
	res, err = agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: target(wb, "dst"), Path: "report.txt", As: "in/report.txt"})
	if err != nil {
		t.Fatalf("push of a file: %v", err)
	}
	if res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("push of a file: %+v", res)
	}
	if got := readFile(t, wb.dirs["dst"], "in/report.txt"); !bytes.Equal(got, big) {
		t.Fatal("pushed file differs")
	}
	res, err = agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: target(wb, "dst"), Path: "proj"})
	if err != nil {
		t.Fatalf("push of a folder: %v", err)
	}
	if res.Outcome != ops.OutcomeDelivered || res.Files != 3 {
		t.Fatalf("push of a folder: %+v", res)
	}
	for _, rel := range []string{"proj/a.txt", "proj/sub/b.bin", "proj/sub/deeper/c.txt"} {
		if !bytes.Equal(readFile(t, wb.dirs["dst"], rel), readFile(t, wa.dirs["src"], rel)) {
			t.Fatalf("%s differs after the folder push", rel)
		}
	}

	// The owner's history has the four transfers, delivered.
	defer func() {
		if p := c.receiptErr.Load(); p != nil {
			t.Errorf("a receipt failed: %v", *p)
		}
		if t.Failed() {
			for _, r := range e.transfers() {
				t.Logf("record: %+v", r)
			}
		}
	}()
	waitFor(t, "four delivered transfers", func() bool {
		n := 0
		for _, r := range e.transfers() {
			if r.State == ops.FileStateDelivered {
				n++
			}
		}
		return n == 4
	})
}
