//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// WP11 security suite: confinement refusals on both ends, the actions of the
// caller (no action, no transfer), the Worker's local switch, the
// no-overwrite rule and the integrity check of both receivers. Every refusal
// leaves nothing published.

func TestFilesConfinementRefusalsOfTheTargetWorker(t *testing.T) {
	e := startFilesE2E(t)
	w := e.startWorker("wa", fxWorkerOptions{}, "src")
	bearer := e.key("files", fileActions, w)
	src := w.dirs["src"]
	outside := realDir(t, t.TempDir())
	writeFile(t, outside, "secret.txt", []byte("outside the root"))
	writeFile(t, src, "ok.txt", []byte("inside"))
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "linkdir")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "repo/.git/config", []byte("[core]\n"))

	refused := func(op string, args any, want ...proto.Code) {
		t.Helper()
		var res ops.FileTransferResult
		err := e.call(bearer, op, args, &res)
		got := codeOf(err)
		for _, w := range want {
			if got == w {
				return
			}
		}
		t.Fatalf("%s %+v: got %v (result %+v), want one of %v", op, args, err, res, want)
	}
	tgt := target(w, "src")
	// The wire path forms: .., absolute, empty components.
	for _, p := range []string{"../secret.txt", "/etc/passwd", "a/../../x", "./ok.txt", "a//b"} {
		refused(ops.OpFilePull, ops.FilePullArgs{Target: tgt, Path: p}, proto.CodePathOutsideRoot, proto.CodeInvalidArgument)
	}
	// A symlink, as the file and as a folder on the way.
	refused(ops.OpFilePull, ops.FilePullArgs{Target: tgt, Path: "link.txt"}, proto.CodeFileTypeRefused, proto.CodePathOutsideRoot)
	refused(ops.OpFilePull, ops.FilePullArgs{Target: tgt, Path: "linkdir/secret.txt"}, proto.CodePathOutsideRoot, proto.CodeFileTypeRefused)
	refused(ops.OpFileList, ops.FileListArgs{Target: tgt, Path: "linkdir"}, proto.CodePathOutsideRoot, proto.CodeFileTypeRefused)
	// Protected destinations of a push: .git internals.
	for _, as := range []string{".git/hooks/pre-commit", "repo/.git/hooks/post-checkout", "repo/.git/config"} {
		refused(ops.OpFilePush, map[string]any{"target": tgt, "path": "x", "as": as, "kind": "file", "size": 1}, proto.CodePathProtected)
	}
	// The push destination path forms.
	for _, as := range []string{"../escape.txt", "/tmp/escape.txt"} {
		refused(ops.OpFilePush, map[string]any{"target": tgt, "path": "x", "as": as, "kind": "file", "size": 1}, proto.CodePathOutsideRoot, proto.CodeInvalidArgument)
	}
	// The caller may not set the Hub's members.
	refused(ops.OpFilePull, map[string]any{"target": tgt, "path": "ok.txt", "transfer_id": ops.NewUUIDv7()}, proto.CodeInvalidArgument)

	// The caller end refuses a destination outside its own root before the
	// network (FR-4).
	c := e.caller(bearer)
	if _, err := agent.CallerPull(ctxT(t), c.FileCaller, ops.FilePullArgs{Target: tgt, Path: "ok.txt", To: "../x"}); codeOf(err) != proto.CodePathOutsideRoot {
		t.Fatalf("pull --to ../x: %v", err)
	}
	if exists(filepath.Dir(c.dir), "x") {
		t.Fatal("a file was written outside the caller's root")
	}
	// The secret never left: nothing outside was read or written.
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "outside the root" {
		t.Fatal("the outside file changed")
	}
	if exists(src, ".git") || exists(src, "repo/.git/hooks") {
		t.Fatal("a protected folder was created")
	}
}

func TestFilesActionsOfTheCallerAndTheWorkerSwitch(t *testing.T) {
	e := startFilesE2E(t)
	w := e.startWorker("wa", fxWorkerOptions{}, "src")
	writeFile(t, w.dirs["src"], "a.txt", []byte("alpha"))
	tgt := target(w, "src")
	ctx := ctxT(t)

	// No file action: no listing, no pull, no push.
	readOnly := e.key("read-only", []ops.Action{ops.ActionRead, ops.ActionOperate}, w)
	c := e.caller(readOnly)
	writeFile(t, c.dir, "up.txt", []byte("upload"))
	if err := e.call(readOnly, ops.OpFileList, ops.FileListArgs{Target: tgt}, nil); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("file_list without file_read: %v", err)
	}
	if _, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: tgt, Path: "a.txt"}); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("pull without file_read: %v", err)
	}
	if _, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "up.txt"}); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("push without file_write: %v", err)
	}
	// Pull only: push is forbidden.
	pullOnly := e.key("pull-only", []ops.Action{ops.ActionRead, ops.ActionFileRead}, w)
	c = e.caller(pullOnly)
	writeFile(t, c.dir, "up.txt", []byte("upload"))
	if res, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: tgt, Path: "a.txt"}); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pull with file_read: %+v %v", res, err)
	}
	if _, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "up.txt"}); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("push with file_read only: %v", err)
	}
	// Push only: pull and list are forbidden.
	pushOnly := e.key("push-only", []ops.Action{ops.ActionRead, ops.ActionFileWrite}, w)
	c = e.caller(pushOnly)
	writeFile(t, c.dir, "up.txt", []byte("upload"))
	if _, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: tgt, Path: "a.txt"}); codeOf(err) != proto.CodeForbidden {
		t.Fatalf("pull with file_write only: %v", err)
	}
	if res, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "up.txt"}); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("push with file_write: %+v %v", res, err)
	}
	// An "everything" key without named file actions has none (FR-11) is
	// covered by the authz tests; a key of another user's Worker is too.

	// The Worker's own switch wins over the Hub (SEC-3).
	all := e.key("all", fileActions, w)
	c = e.caller(all)
	writeFile(t, c.dir, "up2.txt", []byte("upload 2"))
	w.setMode(ops.FileModeOff)
	if _, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: tgt, Path: "a.txt"}); codeOf(err) != proto.CodeFilesDisabled {
		t.Fatalf("pull from a switched-off Worker: %v", err)
	}
	if _, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "up2.txt"}); codeOf(err) != proto.CodeFilesDisabled {
		t.Fatalf("push to a switched-off Worker: %v", err)
	}
	if err := e.call(all, ops.OpFileList, ops.FileListArgs{Target: tgt}, nil); codeOf(err) != proto.CodeFilesDisabled {
		t.Fatalf("file_list of a switched-off Worker: %v", err)
	}
	// send_only: the Worker sends (a pull from it) but receives nothing.
	w.setMode(ops.FileModeSendOnly)
	if res, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: tgt, Path: "a.txt", To: "a2.txt"}); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pull from a send_only Worker: %+v %v", res, err)
	}
	if _, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "up2.txt"}); codeOf(err) != proto.CodeFilesDisabled {
		t.Fatalf("push to a send_only Worker: %v", err)
	}
	if exists(w.dirs["src"], "up2.txt") {
		t.Fatal("a refused push was published")
	}
	w.setMode(ops.FileModeOn)
}

func TestFilesOverwriteIsRefusedUnlessAsked(t *testing.T) {
	e := startFilesE2E(t)
	w := e.startWorker("wa", fxWorkerOptions{}, "src")
	bearer := e.key("files", fileActions, w)
	c := e.caller(bearer)
	ctx := ctxT(t)
	tgt := target(w, "src")
	writeFile(t, w.dirs["src"], "same.txt", []byte("target's own"))
	writeFile(t, c.dir, "same.txt", []byte("caller's own"))

	if _, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "same.txt"}); codeOf(err) != proto.CodeConflict {
		t.Fatalf("push over an existing file: %v", err)
	}
	if string(readFile(t, w.dirs["src"], "same.txt")) != "target's own" {
		t.Fatal("the existing file was replaced")
	}
	// The caller end finds the conflict when it publishes: the transfer
	// ends failed_not_sent with the reason, and the file stays.
	if res, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: tgt, Path: "same.txt"}); codeOf(err) != proto.CodeConflict &&
		(err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != string(proto.CodeConflict)) {
		t.Fatalf("pull over an existing file: %+v %v", res, err)
	}
	if string(readFile(t, c.dir, "same.txt")) != "caller's own" {
		t.Fatal("the caller's file was replaced")
	}
	res, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "same.txt", Overwrite: true})
	if err != nil || res.Outcome != ops.OutcomeDelivered || string(readFile(t, w.dirs["src"], "same.txt")) != "caller's own" {
		t.Fatalf("push with overwrite: %+v %v", res, err)
	}
	// A folder push merges into an existing folder (directories are
	// created one component at a time, FR-7); an existing file in it is
	// never replaced without overwrite.
	writeFile(t, w.dirs["src"], "dir/x.txt", []byte("x of the target"))
	writeFile(t, c.dir, "dir/y.txt", []byte("y"))
	if res, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "dir"}); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("push of a folder into a folder: %+v %v", res, err)
	}
	if string(readFile(t, w.dirs["src"], "dir/x.txt")) != "x of the target" || string(readFile(t, w.dirs["src"], "dir/y.txt")) != "y" {
		t.Fatal("the folder push did not merge")
	}
	writeFile(t, c.dir, "dir/x.txt", []byte("x of the caller"))
	res, err = agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "dir"})
	if err == nil && res.Outcome == ops.OutcomeDelivered {
		t.Fatalf("a folder push replaced existing files: %+v", res)
	}
	if string(readFile(t, w.dirs["src"], "dir/x.txt")) != "x of the target" {
		t.Fatal("an existing file of the folder was replaced")
	}
}

// flipTransport changes one byte of the body after skip bytes: of the
// response (a pull) or of the request (a push).
type flipTransport struct {
	base    http.RoundTripper
	skip    int64
	request bool
}

func (f flipTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.request && r.Body != nil {
		r = r.Clone(r.Context())
		r.Body = &flipBody{rc: r.Body, skip: f.skip}
	}
	resp, err := f.base.RoundTrip(r)
	if err != nil || f.request {
		return resp, err
	}
	resp.Body = &flipBody{rc: resp.Body, skip: f.skip}
	return resp, nil
}

type flipBody struct {
	rc   io.ReadCloser
	skip int64
	n    int64
	done bool
}

func (b *flipBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if !b.done && b.n+int64(n) > b.skip {
		p[b.skip-b.n] ^= 0x01
		b.done = true
	}
	b.n += int64(n)
	return n, err
}

func (b *flipBody) Close() error { return b.rc.Close() }

func TestFilesIntegrityFailurePublishesNothingOnEitherReceiver(t *testing.T) {
	e := startFilesE2E(t)
	w := e.startWorker("wa", fxWorkerOptions{}, "src")
	bearer := e.key("files", fileActions, w)
	ctx := ctxT(t)
	tgt := target(w, "src")
	content := noise(512 << 10) // identity coded: the flipped byte is a content byte
	writeFile(t, w.dirs["src"], "data.bin", content)
	writeFile(t, w.dirs["src"], "tree/a.bin", content)
	writeFile(t, w.dirs["src"], "tree/b.txt", []byte("b"))

	corrupt := func(c *fxCaller, request bool) {
		leg := c.Leg
		c.Leg = func(ctx context.Context, adm ops.FileTransferResult) (*agent.FileLeg, error) {
			l, err := leg(ctx, adm)
			if err != nil {
				return nil, err
			}
			cl := *l.Client
			cl.Transport = flipTransport{base: l.Client.Transport, skip: 100 << 10, request: request}
			l.Client = &cl
			return l, nil
		}
	}
	// Pull: the caller verifies and refuses.
	c := e.caller(bearer)
	corrupt(c, false)
	for _, p := range []string{"data.bin", "tree"} {
		res, err := agent.CallerPull(ctx, c.FileCaller, ops.FilePullArgs{Target: tgt, Path: p})
		if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != string(proto.CodeIntegrityFailed) {
			t.Fatalf("a corrupted pull of %s: %+v %v", p, res, err)
		}
		if exists(c.dir, p) {
			t.Fatalf("a corrupted pull of %s was published", p)
		}
	}
	// Push: the target verifies and refuses.
	c = e.caller(bearer)
	writeFile(t, c.dir, "up.bin", content)
	writeFile(t, c.dir, "uptree/a.bin", content)
	corrupt(c, true)
	for _, p := range []string{"up.bin", "uptree"} {
		res, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: p})
		if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != string(proto.CodeIntegrityFailed) {
			t.Fatalf("a corrupted push of %s: %+v %v", p, res, err)
		}
		if exists(w.dirs["src"], p) {
			t.Fatalf("a corrupted push of %s was published", p)
		}
	}
	// The same transfers without the corruption succeed.
	c = e.caller(bearer)
	writeFile(t, c.dir, "up.bin", content)
	if res, err := agent.CallerPush(ctx, c.FileCaller, ops.FilePushArgs{Target: tgt, Path: "up.bin"}); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("clean push: %+v %v", res, err)
	}
	if !bytes.Equal(readFile(t, w.dirs["src"], "up.bin"), content) {
		t.Fatal("clean push differs")
	}
}
