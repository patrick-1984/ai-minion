//go:build linux

package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// fxHost is the fake host with pane folders: every pane's folder is dir, and
// its process is a child that runs there (the root proof reads
// /proc/<pid>/cwd).
type fxHost struct {
	*fakeHost
	pid int
	dir string
}

func (h *fxHost) PaneDir(context.Context, term.PaneRef) (term.PaneDir, error) {
	return term.PaneDir{PID: h.pid, Path: h.dir}, nil
}

// fxFixture is an executor with a file transfer end over a folder.
type fxFixture struct {
	*env
	t     *testing.T
	dir   string
	files *Files
	mode  atomic.Value // string
	link  *Link
	ctx   context.Context
	now   atomic.Int64 // offset of the clock, ns
}

func newFxFixture(t *testing.T, mut ...func(*FilesConfig)) *fxFixture {
	t.Helper()
	filexfer.SetDefaultResolveMode(filexfer.ResolveOpenat2)
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "120")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep child for the pane folder:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	fx := &fxFixture{t: t, dir: dir}
	fx.env = newEnv(t, func(c *Config) { c.Host = &fxHost{fakeHost: c.Host.(*fakeHost), pid: cmd.Process.Pid, dir: dir} })
	fx.host.addSession("work", "bash")
	fx.mode.Store(ops.FileModeOn)
	cfg := FilesConfig{
		Mode:         func() string { return fx.mode.Load().(string) },
		MinFreeBytes: -1,
		Now:          func() time.Time { return time.Now().Add(time.Duration(fx.now.Load())) },
	}
	for _, m := range mut {
		m(&cfg)
	}
	fx.files = NewFiles(fx.ex, cfg)
	fx.ex.AttachFiles(fx.files)
	fx.link = &Link{}
	fx.ctx = withLink(context.Background(), fx.link)
	return fx
}

func (fx *fxFixture) write(rel, content string) {
	fx.t.Helper()
	p := filepath.Join(fx.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fxFixture) exec(op string, actions []ops.Action, args any) (json.RawMessage, error) {
	fx.t.Helper()
	return fx.ex.Execute(fx.ctx, Request{Op: op, Actions: actions, Args: mustJSON(fx.t, args)})
}

func legArgs(t *testing.T) (ops.FileLegArgs, string) {
	t.Helper()
	tok, err := proto.NewLegToken()
	if err != nil {
		t.Fatal(err)
	}
	return ops.FileLegArgs{TransferID: ops.NewUUIDv7(), LegToken: tok}, tok
}

var (
	fileRead  = []ops.Action{ops.ActionFileRead}
	fileWrite = []ops.Action{ops.ActionFileWrite}
	fxTarget  = ops.Target{SessionName: "work"}
)

func (fx *fxFixture) pull(path string) (ops.FileTransferResult, ops.FileLegArgs, error) {
	leg, _ := legArgs(fx.t)
	raw, err := fx.exec(ops.OpFilePull, fileRead, ops.FilePullArgs{Target: fxTarget, Path: path, FileLegArgs: leg})
	var res ops.FileTransferResult
	if err == nil {
		if err := json.Unmarshal(raw, &res); err != nil {
			fx.t.Fatal(err)
		}
	}
	return res, leg, err
}

func (fx *fxFixture) push(as, kind string, size int64, overwrite bool) (ops.FileTransferResult, ops.FileLegArgs, error) {
	leg, _ := legArgs(fx.t)
	raw, err := fx.exec(ops.OpFilePush, fileWrite, ops.FilePushArgs{Target: fxTarget, Path: "local/" + as, As: as, Kind: kind, Size: size, Entries: 1, Overwrite: overwrite, FileLegArgs: leg})
	var res ops.FileTransferResult
	if err == nil {
		if err := json.Unmarshal(raw, &res); err != nil {
			fx.t.Fatal(err)
		}
	}
	return res, leg, err
}

func codeOf(t *testing.T, err error) (proto.Code, proto.Code) {
	t.Helper()
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("not an API error: %v", err)
	}
	return ae.Code, ae.Reason
}

func TestFilesExecutorMatrix(t *testing.T) {
	fx := newFxFixture(t)
	fx.write("a.txt", "hello")
	cases := []struct {
		mode             string
		list, pull, push proto.Code
	}{
		{ops.FileModeOn, "", "", ""},
		{ops.FileModeSendOnly, "", "", proto.CodeFilesDisabled},
		{ops.FileModeReceiveOnly, "", proto.CodeFilesDisabled, ""},
		{ops.FileModeOff, proto.CodeFilesDisabled, proto.CodeFilesDisabled, proto.CodeFilesDisabled},
		{"bogus", proto.CodeFilesDisabled, proto.CodeFilesDisabled, proto.CodeFilesDisabled},
	}
	for _, c := range cases {
		fx.mode.Store(c.mode)
		_, err := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget})
		check := func(what string, want proto.Code, err error) {
			t.Helper()
			if want == "" {
				if err != nil {
					t.Errorf("mode %s %s: %v", c.mode, what, err)
				}
				return
			}
			if got, _ := codeOf(t, err); got != want {
				t.Errorf("mode %s %s: code %s, want %s", c.mode, what, got, want)
			}
		}
		check("list", c.list, err)
		_, _, err = fx.pull("a.txt")
		check("pull", c.pull, err)
		_, _, err = fx.push(fmt.Sprintf("new-%s.txt", c.mode), ops.FileKindFile, 3, false)
		check("push", c.push, err)
		fx.files.DropConn(fx.link) // free the transfer slots
	}
	fx.mode.Store(ops.FileModeOn)

	// preview_only refuses receiving and allows sending.
	fx.policy.SetPreviewOnly(true)
	if _, _, err := fx.push("p.txt", ops.FileKindFile, 1, false); err == nil {
		t.Fatal("push on a preview-only Worker was admitted")
	} else if code, reason := codeOf(t, err); code != proto.CodeForbidden || reason != proto.CodePreviewOnly {
		t.Fatalf("push on preview-only: %s/%s", code, reason)
	}
	if _, _, err := fx.pull("a.txt"); err != nil {
		t.Fatalf("pull on preview-only: %v", err)
	}
	fx.files.DropConn(fx.link)
	fx.policy.SetPreviewOnly(false)

	// Missing actions: file_write does not list or pull, file_read does not push.
	if _, err := fx.exec(ops.OpFileList, fileWrite, ops.FileListArgs{Target: fxTarget}); err == nil {
		t.Fatal("list with file_write only")
	}
	if _, err := fx.exec(ops.OpFilePush, fileRead, ops.FilePushArgs{Target: fxTarget, As: "x", Kind: ops.FileKindFile}); err == nil {
		t.Fatal("push with file_read only")
	} else if code, _ := codeOf(t, err); code != proto.CodeForbidden {
		t.Fatalf("push with file_read: %s", code)
	}

	// Not shared.
	if err := fx.st.SaveShare(store.ShareSelection{Mode: store.ShareNone}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fx.pull("a.txt"); err == nil {
		t.Fatal("pull of an unshared session")
	} else if _, reason := codeOf(t, err); reason != proto.CodeNotShared {
		t.Fatalf("unshared: reason %s", reason)
	}
	if err := fx.st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}

	// Without the Hub's transfer id there is nothing to prepare.
	if _, err := fx.exec(ops.OpFilePull, fileRead, ops.FilePullArgs{Target: fxTarget, Path: "a.txt"}); err == nil {
		t.Fatal("pull without a transfer id")
	} else if code, _ := codeOf(t, err); code != proto.CodeInvalidArgument {
		t.Fatalf("no transfer id: %s", code)
	}

	// Hostile and sensitive paths.
	for _, p := range []string{"../x", "/etc/passwd", "a/../../b", "a\\..\\b", "a/./b"} {
		if _, _, err := fx.pull(p); err == nil {
			t.Fatalf("pull %q admitted", p)
		} else if code, _ := codeOf(t, err); code != proto.CodePathOutsideRoot {
			t.Fatalf("pull %q: %s", p, code)
		}
	}
	fx.write(".ssh/id_ed25519", "secret")
	if _, _, err := fx.pull(".ssh/id_ed25519"); err == nil {
		t.Fatal("pull of a sensitive file")
	} else if code, _ := codeOf(t, err); code != proto.CodePathSensitive {
		t.Fatalf("sensitive: %s", code)
	}
	if _, _, err := fx.push(".git/hooks/pre-commit", ops.FileKindFile, 1, false); err == nil {
		t.Fatal("push into .git/hooks")
	} else if code, _ := codeOf(t, err); code != proto.CodePathProtected {
		t.Fatalf("protected: %s", code)
	}
	if _, _, err := fx.push("a.txt", ops.FileKindFile, 1, false); err == nil {
		t.Fatal("push over an existing file without overwrite")
	} else if code, _ := codeOf(t, err); code != proto.CodeConflict {
		t.Fatalf("existing: %s", code)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(fx.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fx.pull("link"); err == nil {
		t.Fatal("pull of a symlink")
	} else if code, _ := codeOf(t, err); code != proto.CodeFileTypeRefused {
		t.Fatalf("symlink: %s", code)
	}
}

func TestFilesUnavailableAnswersRootUnknown(t *testing.T) {
	fx := newFxFixture(t, func(c *FilesConfig) {
		c.Unavailable = "File transfer is not available on Windows Workers in this version"
	})
	if fx.files.Announce() || fx.ex.filesAnnounced() {
		t.Fatal("an unavailable end announces files.v1")
	}
	for _, f := range fx.ex.Features() {
		if f == proto.FeatureFilesV1 {
			t.Fatal("files.v1 in the features")
		}
	}
	for _, op := range fx.ex.Ops() {
		if ops.IsFileOp(op) {
			t.Fatalf("file op %s announced without files.v1", op)
		}
	}
	if _, err := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget}); err == nil {
		t.Fatal("list on an unavailable end")
	} else if code, _ := codeOf(t, err); code != proto.CodeFilesRootUnknown {
		t.Fatalf("code %s", code)
	}

	ok := newFxFixture(t)
	if !ok.files.Announce() {
		t.Fatal("an available end does not announce files.v1:", ok.files.Status())
	}
	found := false
	for _, f := range ok.ex.Features() {
		found = found || f == proto.FeatureFilesV1
	}
	if !found {
		t.Fatal("files.v1 missing from the features")
	}
}

func TestFilesRootFloor(t *testing.T) {
	fx := newFxFixture(t, func(c *FilesConfig) {
		c.Floor = func() *filexfer.Floor { return nil }
	})
	home := fx.dir
	fx.files.cfg.Floor = func() *filexfer.Floor { return filexfer.NewFloor(filexfer.FloorConfig{Home: home}) }
	if _, err := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget}); err == nil {
		t.Fatal("home as the root was accepted")
	} else if code, _ := codeOf(t, err); code != proto.CodeFilesRootRefused {
		t.Fatalf("home: %s", code)
	}
	fx.files.cfg.Floor = func() *filexfer.Floor { return filexfer.NewFloor(filexfer.FloorConfig{Home: home, AllowHome: true}) }
	if _, err := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget}); err != nil {
		t.Fatalf("home allowed: %v", err)
	}
}

func TestFilesListPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 10,000 files")
	}
	fx := newFxFixture(t)
	const n = 10000
	want := make([]string, 0, n)
	for i := range n {
		name := fmt.Sprintf("%05d", i) + strings.Repeat("n", proto.MaxNameBytes-5)
		if err := os.WriteFile(filepath.Join(fx.dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		want = append(want, name)
	}
	sort.Strings(want)
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("the listing does not end")
		}
		raw, err := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget, Cursor: cursor, Limit: proto.FileListMaxLimit})
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > proto.MaxFileControlResult {
			t.Fatalf("page of %d bytes", len(raw))
		}
		var res ops.FileListResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		for _, e := range res.Entries {
			got = append(got, e.Name)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if len(got) != n || !sort.StringsAreSorted(got) || got[0] != want[0] || got[n-1] != want[n-1] {
		t.Fatalf("listed %d entries, want %d sorted", len(got), n)
	}
	// A cursor of another folder is refused.
	if err := os.Mkdir(filepath.Join(fx.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget, Limit: 1})
	var first ops.FileListResult
	_ = json.Unmarshal(raw, &first)
	if _, err := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget, Path: "sub", Cursor: first.NextCursor}); err == nil {
		t.Fatal("a cursor of another folder was accepted")
	}
}

func TestFilesListTypes(t *testing.T) {
	fx := newFxFixture(t)
	fx.write("f.txt", "x")
	fx.write("d/inner", "y")
	fx.write(".ssh/config", "z")
	_ = os.Symlink("f.txt", filepath.Join(fx.dir, "sym"))
	_ = os.Link(filepath.Join(fx.dir, "f.txt"), filepath.Join(fx.dir, "hard"))
	raw, err := fx.exec(ops.OpFileList, fileRead, ops.FileListArgs{Target: fxTarget})
	if err != nil {
		t.Fatal(err)
	}
	var res ops.FileListResult
	_ = json.Unmarshal(raw, &res)
	got := map[string]string{}
	for _, e := range res.Entries {
		got[e.Name] = e.Type + "/" + e.SkippedReason
	}
	want := map[string]string{
		".ssh": "dir/sensitive", "d": "dir/", "f.txt": "file/hardlink", "hard": "file/hardlink", "sym": "symlink/symlink",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
}

// legServer serves the data legs of conn over a loopback HTTP server.
func (fx *fxFixture) legServer(conn any) *httptest.Server {
	srv := httptest.NewServer(fx.files.LegHandler(conn))
	fx.t.Cleanup(srv.Close)
	return srv
}

func (fx *fxFixture) get(srv *httptest.Server, leg ops.FileLegArgs, hdr map[string]string) (*http.Response, []byte) {
	fx.t.Helper()
	rq, _ := http.NewRequest(http.MethodGet, srv.URL+proto.FilesLegPath(leg.TransferID), nil)
	rq.Header.Set(proto.HeaderLegToken, leg.LegToken)
	rq.Header.Set("Accept-Encoding", "identity")
	for k, v := range hdr {
		rq.Header.Set(k, v)
	}
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(rq)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestFilesLegTokenBinding(t *testing.T) {
	fx := newFxFixture(t)
	fx.write("a.txt", "hello")
	srv := fx.legServer(fx.link)
	other := fx.legServer(&Link{})

	_, leg, err := fx.pull("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Wrong connection, wrong token, wrong direction: 403, token not used.
	if resp, _ := fx.get(other, leg, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other connection: %d", resp.StatusCode)
	}
	bad := leg
	bad.LegToken, _ = proto.NewLegToken()
	if resp, _ := fx.get(srv, bad, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	rq, _ := http.NewRequest(http.MethodPut, srv.URL+proto.FilesLegPath(leg.TransferID), strings.NewReader("x"))
	rq.Header.Set(proto.HeaderLegToken, leg.LegToken)
	if resp, err := http.DefaultClient.Do(rq); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong direction: %v %v", resp, err)
	}
	// The right leg works once; a second use is transfer_expired.
	if resp, body := fx.get(srv, leg, nil); resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("leg: %d %q", resp.StatusCode, body)
	}
	if resp, _ := fx.get(srv, leg, nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("reused token: %d", resp.StatusCode)
	}
	// Expired: the clock passes expires_at before the leg opens.
	_, leg2, err := fx.pull("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	fx.now.Store(int64(proto.LegOpenWindow + time.Second))
	if resp, _ := fx.get(srv, leg2, nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("expired: %d", resp.StatusCode)
	}
}

func digestOf(b []byte) string { s := sha256.Sum256(b); return proto.FormatDigestField(s[:]) }

func TestFilesRangeAndDigests(t *testing.T) {
	fx := newFxFixture(t)
	content := strings.Repeat("0123456789", 10)
	fx.write("data.bin", content)
	srv := fx.legServer(fx.link)
	sum := sha256.Sum256([]byte(content))
	md, _ := filexfer.SingleFileManifest("data.bin", int64(len(content)), sum).Digest()

	// Unstable (just written): no ETag, no ranges.
	res, leg, err := fx.pull("data.bin")
	if err != nil {
		t.Fatal(err)
	}
	if res.ETag != "" {
		t.Fatal("an unstable file got an ETag")
	}
	resp, body := fx.get(srv, leg, map[string]string{"Range": "bytes=0-9"})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Accept-Ranges") != "none" || string(body) != content {
		t.Fatalf("unstable: %d %q", resp.StatusCode, resp.Header.Get("Accept-Ranges"))
	}

	fx.now.Store(int64(time.Hour)) // the file is now stable
	full := func(hdr map[string]string) (*http.Response, []byte, ops.FileTransferResult) {
		res, leg, err := fx.pull("data.bin")
		if err != nil {
			t.Fatal(err)
		}
		fx.now.Store(int64(time.Hour)) // inside the window
		resp, body := fx.get(srv, leg, hdr)
		return resp, body, res
	}
	resp, body, res = full(nil)
	if res.ETag == "" || resp.Header.Get("ETag") != res.ETag || resp.StatusCode != http.StatusOK || string(body) != content {
		t.Fatalf("stable full: %d etag %q", resp.StatusCode, res.ETag)
	}
	if resp.Trailer.Get(proto.HeaderReprDigest) != digestOf([]byte(content)) ||
		resp.Trailer.Get(proto.HeaderContentDigest) != digestOf(body) ||
		resp.Trailer.Get(proto.HeaderManifestDigest) != proto.FormatDigestField(md[:]) {
		t.Fatalf("trailers: %v", resp.Trailer)
	}
	etag := res.ETag

	resp, body, _ = full(map[string]string{"Range": "bytes=10-19", "If-Range": etag})
	if resp.StatusCode != http.StatusPartialContent || string(body) != content[10:20] ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 10-19/%d", len(content)) {
		t.Fatalf("range: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Range"), body)
	}
	if resp.Trailer.Get(proto.HeaderReprDigest) != digestOf([]byte(content)) || resp.Trailer.Get(proto.HeaderContentDigest) != digestOf(body) {
		t.Fatalf("range trailers: %v", resp.Trailer)
	}

	resp, body, _ = full(map[string]string{"Range": "bytes=10-19", "If-Range": `"other"`})
	if resp.StatusCode != http.StatusOK || string(body) != content {
		t.Fatalf("If-Range mismatch: %d", resp.StatusCode)
	}
	resp, _, _ = full(map[string]string{"Range": "bytes=1000-", "If-Range": etag})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", len(content)) {
		t.Fatalf("unsatisfiable: %d %q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
}

func TestFilesPullFolder(t *testing.T) {
	fx := newFxFixture(t)
	fx.write("proj/a.txt", "a")
	fx.write("proj/sub/b.txt", "bb")
	fx.write("proj/.ssh/key", "secret")
	srv := fx.legServer(fx.link)
	res, leg, err := fx.pull("proj")
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ops.FileKindDir || res.Size != 3 {
		t.Fatalf("admission: %+v", res)
	}
	resp, body := fx.get(srv, leg, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != proto.ContentTypeTar {
		t.Fatalf("folder: %d", resp.StatusCode)
	}
	if resp.Trailer.Get(proto.HeaderContentDigest) != digestOf(body) || resp.Trailer.Get(proto.HeaderManifestDigest) == "" {
		t.Fatalf("trailers: %v", resp.Trailer)
	}
	tr := tar.NewReader(bytes.NewReader(body))
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Contains(strings.Join(names, " "), ".ssh") || len(names) < 3 || names[len(names)-1] != proto.ManifestName {
		t.Fatalf("archive: %v", names)
	}
}

// pushBody is a request body that runs hook when it reaches its end.
type pushBody struct {
	r    io.Reader
	hook func()
}

func (b *pushBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) && b.hook != nil {
		b.hook()
		b.hook = nil
	}
	return n, err
}

func (fx *fxFixture) put(srv *httptest.Server, leg ops.FileLegArgs, name string, content []byte, hook func(), badDigest bool) (*http.Response, ops.FileReceipt) {
	fx.t.Helper()
	sum := sha256.Sum256(content)
	md, _ := filexfer.SingleFileManifest(name, int64(len(content)), sum).Digest()
	rq, _ := http.NewRequest(http.MethodPut, srv.URL+proto.FilesLegPath(leg.TransferID), &pushBody{r: bytes.NewReader(content), hook: hook})
	rq.ContentLength = -1
	rq.Header.Set(proto.HeaderLegToken, leg.LegToken)
	rq.Trailer = http.Header{}
	cd := digestOf(content)
	if badDigest {
		cd = digestOf([]byte("other"))
	}
	rq.Trailer.Set(proto.HeaderContentDigest, cd)
	rq.Trailer.Set(proto.HeaderReprDigest, proto.FormatDigestField(sum[:]))
	rq.Trailer.Set(proto.HeaderManifestDigest, proto.FormatDigestField(md[:]))
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var rc ops.FileReceipt
	_ = json.NewDecoder(resp.Body).Decode(&rc)
	return resp, rc
}

func (fx *fxFixture) noParts() {
	fx.t.Helper()
	ents, _ := os.ReadDir(fx.dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), proto.StagingPrefix) {
			fx.t.Fatalf("staging file left: %s", e.Name())
		}
	}
}

func TestFilesPushFile(t *testing.T) {
	fx := newFxFixture(t)
	srv := fx.legServer(fx.link)
	content := []byte("pushed content\n")

	_, leg, err := fx.push("in.txt", ops.FileKindFile, int64(len(content)), false)
	if err != nil {
		t.Fatal(err)
	}
	resp, rc := fx.put(srv, leg, "in.txt", content, nil, false)
	if resp.StatusCode != http.StatusCreated || rc.Outcome != ops.OutcomeDelivered || rc.Files != 1 {
		t.Fatalf("push: %d %+v", resp.StatusCode, rc)
	}
	got, err := os.ReadFile(filepath.Join(fx.dir, "in.txt"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published: %q %v", got, err)
	}
	if st, _ := os.Stat(filepath.Join(fx.dir, "in.txt")); st.Mode().Perm()&0o111 != 0 {
		t.Fatalf("exec bit set: %v", st.Mode())
	}

	// A wrong content digest publishes nothing.
	_, leg, _ = fx.push("bad.txt", ops.FileKindFile, int64(len(content)), false)
	resp, rc = fx.put(srv, leg, "bad.txt", content, nil, true)
	if rc.Outcome != ops.OutcomeFailedNotSent || rc.Reason != string(proto.CodeIntegrityFailed) || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("bad digest: %d %+v", resp.StatusCode, rc)
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "bad.txt")); err == nil {
		t.Fatal("a failed push published")
	}

	// The switch turned off while the body arrives prevents the publish.
	_, leg, _ = fx.push("late.txt", ops.FileKindFile, int64(len(content)), false)
	resp, rc = fx.put(srv, leg, "late.txt", content, func() { fx.mode.Store(ops.FileModeOff) }, false)
	if rc.Outcome != ops.OutcomeFailedNotSent || rc.Reason != string(proto.CodeFilesDisabled) {
		t.Fatalf("switched off mid-push: %d %+v", resp.StatusCode, rc)
	}
	if _, err := os.Stat(filepath.Join(fx.dir, "late.txt")); err == nil {
		t.Fatal("published after the switch was turned off")
	}
	fx.noParts()
}

func TestParseByteRange(t *testing.T) {
	cases := []struct {
		v         string
		s, e      int64
		ok, unsat bool
	}{
		{"bytes=0-9", 0, 9, true, false},
		{"bytes=90-", 90, 99, true, false},
		{"bytes=-10", 90, 99, true, false},
		{"bytes=95-200", 95, 99, true, false},
		{"bytes=100-", 0, 0, false, true},
		{"bytes=-0", 0, 0, false, true},
		{"bytes=0-1,5-6", 0, 0, false, false},
		{"items=0-1", 0, 0, false, false},
		{"bytes=5-1", 0, 0, false, false},
	}
	for _, c := range cases {
		s, e, ok, unsat := parseByteRange(c.v, 100)
		if ok != c.ok || unsat != c.unsat || (ok && (s != c.s || e != c.e)) {
			t.Errorf("%s: %d-%d ok=%v unsat=%v", c.v, s, e, ok, unsat)
		}
	}
}
