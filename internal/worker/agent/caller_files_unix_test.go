//go:build linux || darwin

package agent

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func callerRoot(t *testing.T) (*filexfer.Root, string) {
	t.Helper()
	filexfer.SetDefaultResolveMode(filexfer.ResolveWalk)
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filexfer.OpenDirRoot(dir, filexfer.RootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// TestCallerPullRefusesADestinationOutsideTheRootBeforeAdmission: --to ../x is
// path_outside_root and nothing is sent (BRD FR-4).
func TestCallerPullRefusesADestinationOutsideTheRootBeforeAdmission(t *testing.T) {
	root, _ := callerRoot(t)
	sent := false
	c := FileCaller{Root: root, Admit: func(context.Context, string, any) (ops.FileTransferResult, error) {
		sent = true
		return ops.FileTransferResult{}, nil
	}}
	for _, to := range []string{"../x", "/etc/x", "a/../../b", `a\..\b`} {
		_, err := CallerPull(context.Background(), c, ops.FilePullArgs{Target: ops.Target{WorkerID: "w2", SessionName: "s"}, Path: "a.txt", To: to})
		if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodePathOutsideRoot {
			t.Fatalf("to %q: err %v, want path_outside_root", to, err)
		}
	}
	if sent {
		t.Fatal("the admission was sent")
	}
}

// TestCallerPullDisabledLocally: a caller whose switch refuses receiving gets
// files_disabled before admission (FR-10).
func TestCallerPullDisabledLocally(t *testing.T) {
	root, _ := callerRoot(t)
	c := FileCaller{Root: root,
		Allow: func(e string) error {
			if e == ops.FileEffectReceive {
				return apiErr(proto.CodeFilesDisabled, "", "off")
			}
			return nil
		},
		Admit: func(context.Context, string, any) (ops.FileTransferResult, error) {
			t.Fatal("admission sent")
			return ops.FileTransferResult{}, nil
		}}
	_, err := CallerPull(context.Background(), c, ops.FilePullArgs{Target: ops.Target{WorkerID: "w2", SessionName: "s"}, Path: "a.txt"})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeFilesDisabled {
		t.Fatalf("err %v, want files_disabled", err)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func admitted(kind string, size int64) ops.FileTransferResult {
	tok, _ := proto.NewLegToken()
	return ops.FileTransferResult{Stage: ops.FileStageAdmitted, TransferID: ops.NewUUIDv7(), Direction: ops.FileDirectionPull,
		Kind: kind, Size: size, LegToken: tok, TargetRoot: "~/x"}
}

// TestCallerPullCancelledLeavesNothingPublished: a pull cancelled while the
// body arrives (Ctrl-C) publishes nothing, leaves no staged file and still
// reports a failed_not_sent receipt (NFR-7, FR-7).
func TestCallerPullCancelledLeavesNothingPublished(t *testing.T) {
	root, dir := callerRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adm := admitted(ops.FileKindFile, 1<<20)
	var receipt *ops.FileReceiptArgs
	c := FileCaller{Root: root, MinFreeBytes: -1,
		Admit: func(context.Context, string, any) (ops.FileTransferResult, error) { return adm, nil },
		Leg: func(context.Context, ops.FileTransferResult) (*FileLeg, error) {
			pr, pw := io.Pipe()
			go func() {
				_, _ = pw.Write(make([]byte, 64<<10))
				cancel()
				_ = pw.CloseWithError(context.Canceled)
			}()
			cl := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get(proto.HeaderLegToken) != adm.LegToken {
					t.Error("the leg token is missing")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {proto.ContentTypeFile}}, Body: pr, Request: r}, nil
			})}
			return &FileLeg{Client: cl, URL: "http://fx/" + adm.TransferID, Close: func() {}}, nil
		},
		Receipt: func(_ context.Context, a ops.FileReceiptArgs) { receipt = &a },
	}
	res, err := CallerPull(ctx, c, ops.FilePullArgs{Target: ops.Target{WorkerID: "w2", SessionName: "s"}, Path: "big.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ops.OutcomeFailedNotSent || res.LegToken != "" || res.Stage != ops.FileStageCompleted {
		t.Fatalf("result %+v", res)
	}
	if receipt == nil || receipt.Outcome != ops.OutcomeFailedNotSent || receipt.LegToken != adm.LegToken {
		t.Fatalf("receipt %+v", receipt)
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 0 {
		t.Fatalf("left %d entries in the folder, first %q", len(names), names[0].Name())
	}
}

// TestCallerPushRefusesSensitiveSource: a key file is never sent.
func TestCallerPushRefusesSensitiveSource(t *testing.T) {
	root, dir := callerRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := FileCaller{Root: root, Admit: func(context.Context, string, any) (ops.FileTransferResult, error) {
		t.Fatal("admission sent")
		return ops.FileTransferResult{}, nil
	}}
	_, err := CallerPush(context.Background(), c, ops.FilePushArgs{Target: ops.Target{WorkerID: "w2", SessionName: "s"}, Path: "id_ed25519"})
	if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodePathSensitive {
		t.Fatalf("err %v, want path_sensitive", err)
	}
}

// TestReceiptOutboxResendsAfterRestart: a receipt the Hub did not answer is
// kept in the state directory and sent by the next process (FR-15).
func TestReceiptOutboxResendsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	tok, _ := proto.NewLegToken()
	a := ops.FileReceiptArgs{TransferID: ops.NewUUIDv7(), LegToken: tok, FileReceipt: ops.FileReceipt{Outcome: ops.OutcomeDelivered, ManifestDigest: strings.Repeat("a", 64)}}
	o := NewReceiptOutbox(dir, func(context.Context, ops.FileReceiptArgs) error {
		return proto.NewError(proto.CodeWorkerOffline, "offline")
	}, nil)
	o.Deliver(context.Background(), a)
	if o.Pending() != 1 {
		t.Fatalf("pending %d after a failed send", o.Pending())
	}
	if st, err := os.Stat(filepath.Join(dir, ReceiptOutboxFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("outbox file: %v %v", st, err)
	}
	var got []string
	o2 := NewReceiptOutbox(dir, func(_ context.Context, r ops.FileReceiptArgs) error {
		got = append(got, r.TransferID)
		return nil
	}, nil)
	if left := o2.Flush(context.Background()); left != 0 || len(got) != 1 || got[0] != a.TransferID {
		t.Fatalf("left %d, sent %v", left, got)
	}
	if o2.Pending() != 0 {
		t.Fatal("the acknowledged receipt is still waiting")
	}
	// A receipt the Hub refuses for good is dropped.
	o3 := NewReceiptOutbox(dir, func(context.Context, ops.FileReceiptArgs) error {
		return proto.NewError(proto.CodeForbidden, "no")
	}, nil)
	o3.Deliver(context.Background(), a)
	if o3.Pending() != 0 {
		t.Fatal("a refused receipt is kept")
	}
}
