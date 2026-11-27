package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

const (
	filesTransferID = "0190a1b2-c3d4-7e5f-8a6b-0000000000f1"
	filesBase       = "/s/pi-1/minion-1"
)

var filesToken = strings.Repeat("A", proto.LegTokenLen-1) + "Q"

// filesWorker is the session Worker announcing files.v1 and the file ops.
func filesWorker() ops.WorkerInfo {
	w := sessionWorker()
	w.Ops = []string{ops.OpFileList, ops.OpFilePull, ops.OpFilePush}
	w.Features = []string{proto.FeatureFilesV1}
	return w
}

func filesOps(t *testing.T) (*Server, *fakeOps) {
	t.Helper()
	s, f := sessionOps(t)
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{filesWorker()}})
	return s, f
}

func TestFilesPanelOnlyWithTheFeatureAndTheAccess(t *testing.T) {
	if !proto.ValidLegToken(filesToken) {
		t.Fatal("the test token is not a leg token")
	}
	// An old Worker: no view at all.
	s, _ := sessionOps(t)
	body := sessGet(t, s, filesBase, sessUser).Body.String()
	mustNotHave(t, body, `id="files-panel"`, "session-files.js", "data-files-get")

	// files.v1 and the owner: the panel loads its listing after the page.
	s, f := filesOps(t)
	body = sessGet(t, s, filesBase, sessUser).Body.String()
	mustHave(t, body, `id="files-panel"`, `hx-post="/s/pi-1/minion-1/files/list" hx-trigger="load"`,
		`<script src="/static/session-files.js" defer></script>`, `id="files-upload"`, `data-files-upload="/s/pi-1/minion-1/files/upload/"`)
	if n := len(f.called(ops.OpFileList)); n != 0 {
		t.Errorf("the page render listed the folder %d times; the panel loads it", n)
	}

	// Without file_push: no upload form.
	w := filesWorker()
	w.Ops = []string{ops.OpFileList, ops.OpFilePull}
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}})
	body = sessGet(t, s, filesBase, sessUser).Body.String()
	mustHave(t, body, `id="files-panel"`)
	mustNotHave(t, body, `id="files-upload"`)

	// Someone who cannot operate the session: one line, no controls.
	w = filesWorker()
	w.OwnerID, w.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}})
	body = sessGet(t, s, filesBase, sessUser).Body.String()
	mustHave(t, body, `id="files-unavailable"`, "You don&#39;t have file access to this session.")
	mustNotHave(t, body, "session-files.js", `id="files-list"`)
	// ... and its routes refuse before any file operation.
	rec := post(t, s, filesBase+"/files/pull", sessUser, url.Values{"path": {"a.txt"}})
	if rec.Code != http.StatusConflict || len(f.called(ops.OpFilePull)) != 0 {
		t.Fatalf("pull without access: %d %s", rec.Code, rec.Body)
	}
}

func TestFilesListEscapesNamesAndPaginates(t *testing.T) {
	s, f := filesOps(t)
	f.reply(ops.OpFileList, ops.FileListResult{Root: "~/projects/<app>", Path: "src", NextCursor: "c_1-x",
		Entries: []ops.FileEntry{
			{Name: `<img src=x onerror=alert(1)>`, Type: proto.FileTypeFile, Size: 2048, MTime: 1764000000},
			{Name: "lib", Type: proto.FileTypeDir},
			{Name: "link", Type: proto.FileTypeSymlink, SkippedReason: proto.SkipSymlink},
		}})
	rec := post(t, s, filesBase+"/files/list", sessUser, url.Values{"path": {"src"}, "cursor": {"c_0"}})
	body := rec.Body.String()
	calls := f.called(ops.OpFileList)
	if len(calls) != 1 || calls[0].Args["path"] != "src" || calls[0].Args["cursor"] != "c_0" {
		t.Fatalf("calls = %+v", calls)
	}
	mustHave(t, body, "&lt;img src=x onerror=alert(1)&gt;", "~/projects/&lt;app&gt;", "2.0 KiB",
		`data-kind="file" data-path="src/&lt;img src=x onerror=alert(1)&gt;"`,
		`<input type="hidden" name="path" value="src/lib">`, `data-kind="dir" data-path="src/lib"`,
		"symbolic link, not followed", `name="cursor" value="c_1-x"`, "Up one folder", `aria-current="location">src</button>`)
	mustNotHave(t, body, "<img src=x", `data-path="src/link"`)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a listing may be cached")
	}

	// A refusal is one line in the listing.
	f.on(ops.OpFileList, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: string(proto.CodeFilesRootRefused), Message: "the root is home"}
	})
	body = post(t, s, filesBase+"/files/list", sessUser, url.Values{}).Body.String()
	mustHave(t, body, `role="alert">The folder of this session cannot be shared`)
}

func TestFilesPullAdmitsAndAnswersTheLeg(t *testing.T) {
	s, f := filesOps(t)
	f.on(ops.OpFilePull, func(args map[string]any) (any, error) {
		return ops.FileTransferResult{Stage: "admitted", TransferID: filesTransferID, Direction: "pull", Kind: ops.FileKindFile,
			Size: 10, ETag: `"e1"`, LegToken: filesToken, LegPath: proto.FilesLegPath(filesTransferID)}, nil
	})
	rec := post(t, s, filesBase+"/files/pull", sessUser, url.Values{"path": {"src/a.txt"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body)
	}
	var a filesAdmission
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.TransferID != filesTransferID || a.LegToken != filesToken || a.LegPath != proto.FilesLegPath(filesTransferID) || a.ETag != `"e1"` || a.Name != "a.txt" {
		t.Fatalf("admission = %+v", a)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("an admission with a token may be cached")
	}
	calls := f.called(ops.OpFilePull)
	if len(calls) != 1 || calls[0].Args["path"] != "src/a.txt" || calls[0].Args["transfer_id"] != nil || calls[0].Args["leg_token"] != nil {
		t.Fatalf("calls = %+v", calls)
	}
	// Hostile paths never reach the Hub.
	for _, p := range []string{"", "../x", "/etc/passwd", "a/../../b", `a\b`, "a//b"} {
		rec := post(t, s, filesBase+"/files/pull", sessUser, url.Values{"path": {p}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: %d", p, rec.Code)
		}
	}
	if len(f.called(ops.OpFilePull)) != 1 {
		t.Error("a hostile path reached the Hub")
	}
	// A refusal keeps its status and says why.
	f.on(ops.OpFilePull, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: string(proto.CodeFilesDisabled), Message: "switched off"}
	})
	rec = post(t, s, filesBase+"/files/pull", sessUser, url.Values{"path": {"a.txt"}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "File transfer is turned off") {
		t.Fatalf("refusal: %d %s", rec.Code, rec.Body)
	}
}

func TestFilesPushAdmitsOneFileIntoTheListedFolder(t *testing.T) {
	s, f := filesOps(t)
	f.on(ops.OpFilePush, func(args map[string]any) (any, error) {
		return ops.FileTransferResult{Stage: "admitted", TransferID: filesTransferID, Direction: "push", Kind: ops.FileKindFile,
			LegToken: filesToken, LegPath: proto.FilesLegPath(filesTransferID)}, nil
	})
	rec := post(t, s, filesBase+"/files/push", sessUser, url.Values{"dir": {"docs"}, "name": {"r.pdf"}, "size": {"42"}, "overwrite": {"1"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	calls := f.called(ops.OpFilePush)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	a := calls[0].Args
	if a["path"] != "r.pdf" || a["as"] != "docs/r.pdf" || a["kind"] != "file" || a["size"] != float64(42) || a["overwrite"] != true {
		t.Fatalf("args = %+v", a)
	}
	// Without the choice an existing file is kept.
	post(t, s, filesBase+"/files/push", sessUser, url.Values{"name": {"r.pdf"}, "size": {"1"}})
	if a := f.called(ops.OpFilePush)[1].Args; a["overwrite"] != nil || a["as"] != "r.pdf" {
		t.Fatalf("args = %+v", a)
	}
	for _, bad := range []url.Values{
		{"name": {"../x"}, "size": {"1"}}, {"name": {"a/b"}, "size": {"1"}}, {"name": {"x"}, "size": {"-1"}},
		{"name": {"x"}}, {"dir": {"../up"}, "name": {"x"}, "size": {"1"}}, {"name": {".minion-part-1"}, "size": {"1"}},
	} {
		if rec := post(t, s, filesBase+"/files/push", sessUser, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, rec.Code)
		}
	}
	if len(f.called(ops.OpFilePush)) != 2 {
		t.Error("a refused upload reached the Hub")
	}
}

// legOps is the fake caller with the in-process data-leg endpoint.
type legOps struct {
	*fakeOps
	got      *http.Request
	body     []byte
	readErr  error
	trailers http.Header
}

func (l *legOps) serveLeg(w http.ResponseWriter, r *http.Request) {
	l.got = r
	l.body, l.readErr = io.ReadAll(r.Body)
	l.trailers = r.Trailer.Clone()
	if l.readErr != nil {
		http.Error(w, "short", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", proto.ContentTypeReceipt)
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"outcome":"delivered","files":1,"bytes":5}`))
}

func uploadReq(name string, body []byte, declared int64) *http.Request {
	req := httptest.NewRequest(http.MethodPut, filesBase+"/files/upload/"+filesTransferID, bytes.NewReader(body))
	req.ContentLength = declared
	req.Header.Set("X-Test-Session", sessUser)
	req.Header.Set(proto.HeaderLegToken, filesToken)
	req.Header.Set(HeaderUploadName, url.PathEscape(name))
	return req
}

func TestFilesUploadSetsTheTrailersOnlyForTheWholeBody(t *testing.T) {
	_, f := filesOps(t)
	l := &legOps{fakeOps: f}
	connect(t, l)
	s := adminServer(t)
	content := []byte("hello")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, uploadReq("a b.txt", content, int64(len(content))))
	if rec.Code != http.StatusCreated || l.got == nil {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if l.got.URL.Path != proto.FilesLegPath(filesTransferID) || l.got.Header.Get(proto.HeaderLegToken) != filesToken || !bytes.Equal(l.body, content) {
		t.Fatalf("leg request %s %v", l.got.URL.Path, l.got.Header)
	}
	sum := sha256.Sum256(content)
	md, err := filexfer.SingleFileManifest("a b.txt", int64(len(content)), sum).Digest()
	if err != nil {
		t.Fatal(err)
	}
	if l.trailers.Get(proto.HeaderContentDigest) != proto.FormatDigestField(sum[:]) ||
		l.trailers.Get(proto.HeaderReprDigest) != proto.FormatDigestField(sum[:]) ||
		l.trailers.Get(proto.HeaderManifestDigest) != proto.FormatDigestField(md[:]) {
		t.Fatalf("trailers = %v", l.trailers)
	}

	// A body shorter than declared (a cancelled upload) gets no trailers.
	l.got, l.trailers = nil, nil
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, uploadReq("a.txt", content, 10))
	if l.got == nil || l.readErr == nil || l.trailers.Get(proto.HeaderManifestDigest) != "" || l.trailers.Get(proto.HeaderContentDigest) != "" {
		t.Fatalf("short body: err %v trailers %v", l.readErr, l.trailers)
	}
	// A longer body fails too.
	l.got, l.trailers = nil, nil
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, uploadReq("a.txt", content, 3))
	if l.got != nil && l.trailers.Get(proto.HeaderManifestDigest) != "" {
		t.Fatalf("long body got trailers %v", l.trailers)
	}
	// Without a token or with a bad name nothing is sent.
	l.got = nil
	for _, mod := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del(proto.HeaderLegToken) },
		func(r *http.Request) { r.Header.Set(HeaderUploadName, url.PathEscape("../x")) },
		func(r *http.Request) { r.Header.Set(HeaderUploadName, "a%2Fb") },
	} {
		req := uploadReq("a.txt", content, int64(len(content)))
		mod(req)
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || l.got != nil {
			t.Errorf("refused upload: %d", rec.Code)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 1 << 30: "1.0 GiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
