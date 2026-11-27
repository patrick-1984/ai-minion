package web

// The Files view of the session page (files.v1, BRD minion-file-transfer
// FR-19, WP9): the folder of the session's pane, listed one page at a time,
// a download of a file (resumable with Range when the Worker gives a strong
// ETag) or of a folder (.tar.gz), and the upload of one file.
//
// The page never handles file content itself, with one exception: the
// upload. Every list and every admission is one operation call as the
// signed-in person (file_list, file_pull, file_push), so the authorization,
// the Hub and Worker policies, the limits and the audit are those of the API.
// The bytes of a download go from the Hub's data-leg endpoint
// (proto.FilesLegPath) straight to the browser's script, which sends the
// caller leg's token in the Minion-Leg-Token header (never in a URL). An
// upload cannot go there directly: a browser cannot send the request
// trailers a receiving Worker verifies (Content-Digest, Repr-Digest and
// Minion-Manifest-Digest). The upload route therefore streams the browser's
// body into the same endpoint in process, hashes it on the way and sets the
// trailers when, and only when, the body has ended with the declared size. A
// cancelled or short upload has no trailers, so the Worker publishes nothing.
//
// Paths are never put in a URL: the list, the admissions and the upload carry
// them in a form body or a request header.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"hash"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Paths of the Files view, under the session path.
const (
	pathFilesList   = "/files/list"
	pathFilesPull   = "/files/pull"
	pathFilesPush   = "/files/push"
	pathFilesUpload = "/files/upload/{transfer}"
)

// HeaderUploadName carries the name of the uploaded file on the upload route,
// percent-encoded (encodeURIComponent): it must equal the last element of the
// path admitted with file_push, which the Worker puts in the manifest.
const HeaderUploadName = "Minion-File-Name"

// maxFilesFormBytes bounds the form of a list or an admission.
const maxFilesFormBytes = 16 << 10

// uploadIdle is how long an upload body may pause before the read fails. The
// Hub's body guard allows a body 30 s in all, which a large upload exceeds;
// the upload route moves the read deadline on while bytes arrive.
var uploadIdle = 60 * time.Second

// filesPaneOrder places the Files view after Details and Access.
const filesPaneOrder = 160

// FilesPanel is what the "files_panel" template sees.
type FilesPanel struct {
	Session string
	Worker  string
	// Unavailable is the one-line reason the panel shows instead of the
	// controls; empty when the panel is usable.
	Unavailable string
	// ListURL, PullURL, PushURL and UploadURL are the routes of the panel
	// (UploadURL ends with "/" and takes the transfer id).
	ListURL, PullURL, PushURL, UploadURL string
	// Push is true when the Worker offers file_push (the upload form is
	// shown); the Hub and the Worker decide again on the admission.
	Push bool
}

// FilesListing is what the "files_list" template sees: one page of one
// folder.
type FilesListing struct {
	ListURL string
	Root    string
	Path    string
	// Crumbs lead back to the root and every folder above Path.
	Crumbs  []FilesCrumb
	Parent  string
	HasUp   bool
	Entries []FilesEntryView
	// Cursor continues the listing of Path; empty on the last page.
	Cursor string
	Error  string
	Push   bool
}

// FilesCrumb is one step of the folder path.
type FilesCrumb struct {
	Label string
	Path  string
}

// FilesEntryView is one row of a listing.
type FilesEntryView struct {
	Name    string
	Path    string
	Dir     bool
	File    bool
	Size    string
	Bytes   int64
	MTime   string
	MTimeAt string
	Skipped string
}

var filesTemplates = fragments("_session_files.html")

// filesAvailability says whether the panel can work for s: the Worker
// announced files.v1 and its file ops, it is online, and the viewer may
// operate the session (an administrator, or the owner: the people who hold
// file_read and file_write, subject to the Hub's decisions).
func filesAvailability(s SessionContext) string {
	switch {
	case !s.Online:
		return "Files are not available while this Worker is offline."
	case !slices.Contains(s.Features, proto.FeatureFilesV1) || !slices.Contains(s.Ops, ops.OpFileList):
		return "This Worker does not offer file transfer (it is older than v0.24, runs on Windows, or turned file transfer off)."
	case !s.Operate:
		return "You don't have file access to this session."
	}
	return ""
}

// renderSessionFiles is the Files view of the session page. It leaves the
// view out for a viewer who cannot use it at all; the listing itself is
// loaded by the panel after the page.
func renderSessionFiles(r *http.Request, s SessionContext) (template.HTML, error) {
	if !slices.Contains(s.Features, proto.FeatureFilesV1) {
		// An older Worker (or one that cannot serve files) gets no view at all.
		return "", nil
	}
	base := SessionPath(r.PathValue("worker"), s.Session)
	p := FilesPanel{Session: s.Session, Worker: s.Worker, Unavailable: filesAvailability(s),
		ListURL: base + pathFilesList, PullURL: base + pathFilesPull, PushURL: base + pathFilesPush,
		UploadURL: base + "/files/upload/",
		Push:      slices.Contains(s.Ops, ops.OpFilePush)}
	var buf bytes.Buffer
	if err := filesTemplates().ExecuteTemplate(&buf, "files_panel", p); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // produced by html/template above
}

// formatBytes writes a size for people: bytes, KiB, MiB, GiB.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTP"[exp]) + "iB"
}

// skipText is the reason an entry cannot be transferred, in words.
func skipText(reason string) string {
	switch reason {
	case "":
		return ""
	case proto.SkipSymlink:
		return "symbolic link, not followed"
	case proto.SkipHardlink:
		return "has several hard links"
	case proto.SkipSpecial:
		return "not a regular file"
	case proto.SkipMount:
		return "on another file system"
	case proto.SkipSensitive:
		return "sensitive, not transferred"
	case proto.SkipProtected:
		return "protected"
	}
	return "cannot be transferred"
}

// joinRel joins a folder and a name of a listing.
func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// crumbsOf lists the folders from the root to dir.
func crumbsOf(dir string) (crumbs []FilesCrumb, parent string) {
	if dir == "" {
		return nil, ""
	}
	parts := strings.Split(dir, "/")
	for i, p := range parts {
		crumbs = append(crumbs, FilesCrumb{Label: p, Path: strings.Join(parts[:i+1], "/")})
	}
	return crumbs, strings.Join(parts[:len(parts)-1], "/")
}

// filesListingOf turns a file_list page into its view.
func filesListingOf(listURL string, res ops.FileListResult, push bool) FilesListing {
	l := FilesListing{ListURL: listURL, Root: res.Root, Path: res.Path, Cursor: res.NextCursor, Push: push}
	l.Crumbs, l.Parent = crumbsOf(res.Path)
	l.HasUp = res.Path != ""
	for _, e := range res.Entries {
		v := FilesEntryView{Name: e.Name, Path: joinRel(res.Path, e.Name), Skipped: skipText(e.SkippedReason)}
		if v.Skipped == "" {
			v.Dir, v.File = e.Type == proto.FileTypeDir, e.Type == proto.FileTypeFile
		}
		if e.Type == proto.FileTypeFile {
			v.Size, v.Bytes = formatBytes(e.Size), e.Size
		}
		if e.MTime > 0 {
			t := time.Unix(e.MTime, 0).UTC()
			v.MTime, v.MTimeAt = t.Format("2006-01-02 15:04"), t.Format(time.RFC3339)
		}
		l.Entries = append(l.Entries, v)
	}
	return l
}

// filesRoute wraps a route of the Files view: a resolved session (a Worker
// the viewer cannot see is a plain 404), the availability of the panel and,
// unless raw is set, a small form body read under its own deadline.
func filesRoute(raw bool, fn func(w http.ResponseWriter, r *http.Request, t target)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !raw {
			r.Body = http.MaxBytesReader(w, r.Body, maxFilesFormBytes)
			if err := r.ParseForm(); err != nil {
				filesJSONError(w, http.StatusBadRequest, "invalid_argument", "The form could not be read.")
				return
			}
		}
		t, err := resolveTarget(r)
		if err != nil {
			var se *StatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				http.NotFound(w, r)
				return
			}
			filesJSONError(w, http.StatusServiceUnavailable, "unavailable", "The Hub could not resolve the session.")
			return
		}
		if why := filesAvailability(sessionContextOf(t)); why != "" {
			filesJSONError(w, http.StatusConflict, "unavailable", why)
			return
		}
		fn(w, r, t)
	})
}

// handleFilesList answers one page of one folder as an HTML fragment.
func handleFilesList(w http.ResponseWriter, r *http.Request, t target) {
	dir := r.PostFormValue("path")
	listURL := t.path() + pathFilesList
	push := slices.Contains(t.worker.Ops, ops.OpFilePush)
	args := ops.FileListArgs{Target: t.opTarget(), Path: dir, Cursor: r.PostFormValue("cursor")}
	var res ops.FileListResult
	if err := t.c.CallOp(r, ops.OpFileList, args, &res); err != nil {
		l := FilesListing{ListURL: listURL, Path: dir, HasUp: dir != "", Error: filesFailure(err), Push: push}
		l.Crumbs, l.Parent = crumbsOf(dir)
		writeFragmentTemplate(w, filesTemplates(), "files_list", l)
		return
	}
	writeFragmentTemplate(w, filesTemplates(), "files_list", filesListingOf(listURL, res, push))
}

// filesFailure is the sentence for a refused or failed file operation.
func filesFailure(err error) string {
	var oe *OpError
	if !errors.As(err, &oe) {
		return "The Hub could not complete the request."
	}
	switch oe.Code {
	case string(proto.CodeFilesDisabled):
		return "File transfer is turned off for this direction on this Worker or this Hub. " + opMessage(oe)
	case string(proto.CodeFilesRootUnknown):
		return "The Worker cannot prove which folder this session is in. " + opMessage(oe)
	case string(proto.CodeFilesRootRefused):
		return "The folder of this session cannot be shared (a system folder, the home folder or a sensitive folder). " + opMessage(oe)
	case "forbidden", "not_found":
		return "You don't have file access to this session."
	}
	return opMessage(oe)
}

// filesAdmission is the JSON answer of a pull or push admission: what the
// page's script needs for the data leg. The token is the browser's own leg
// token; it goes only into the Minion-Leg-Token header.
type filesAdmission struct {
	TransferID string `json:"transfer_id"`
	LegPath    string `json:"leg_path"`
	LegToken   string `json:"leg_token"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size,omitempty"`
	ETag       string `json:"etag,omitempty"`
	Name       string `json:"name"`
}

func writeFilesJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "the answer could not be encoded", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func filesJSONError(w http.ResponseWriter, status int, code, message string) {
	writeFilesJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func filesOpError(w http.ResponseWriter, err error) {
	var oe *OpError
	if errors.As(err, &oe) {
		status := oe.Status
		if status < 400 {
			status = http.StatusBadGateway
		}
		filesJSONError(w, status, oe.Code, filesFailure(err))
		return
	}
	filesJSONError(w, http.StatusBadGateway, "internal", "The Hub could not complete the request.")
}

func admissionOf(res ops.FileTransferResult, name string) (filesAdmission, bool) {
	a := filesAdmission{TransferID: res.TransferID, LegPath: res.LegPath, LegToken: res.LegToken,
		Kind: res.Kind, Size: res.Size, ETag: res.ETag, Name: name}
	if a.LegPath == "" {
		a.LegPath = proto.FilesLegPath(a.TransferID)
	}
	ok := ops.ValidUUIDv7(a.TransferID) && proto.ValidLegToken(a.LegToken) && a.LegPath == proto.FilesLegPath(a.TransferID)
	return a, ok
}

// handleFilesPull admits the download of one file or folder.
func handleFilesPull(w http.ResponseWriter, r *http.Request, t target) {
	p := r.PostFormValue("path")
	if err := ops.CheckFilePath(p, false); err != nil {
		filesJSONError(w, http.StatusBadRequest, string(proto.CodePathOutsideRoot), "Choose a file or folder of the list.")
		return
	}
	var res ops.FileTransferResult
	if err := t.c.CallOp(r, ops.OpFilePull, ops.FilePullArgs{Target: t.opTarget(), Path: p}, &res); err != nil {
		filesOpError(w, err)
		return
	}
	a, ok := admissionOf(res, filexfer.BaseName(p))
	if !ok {
		filesJSONError(w, http.StatusBadGateway, "internal", "The Hub sent an admission the page cannot use.")
		return
	}
	writeFilesJSON(w, http.StatusOK, a)
}

// handleFilesPush admits the upload of one file: into the listed folder
// (dir) under name, with its declared size.
func handleFilesPush(w http.ResponseWriter, r *http.Request, t target) {
	if !slices.Contains(t.worker.Ops, ops.OpFilePush) {
		filesJSONError(w, http.StatusConflict, "unavailable", "This Worker does not accept uploads.")
		return
	}
	name, dir := r.PostFormValue("name"), r.PostFormValue("dir")
	size, err := strconv.ParseInt(r.PostFormValue("size"), 10, 64)
	if err != nil || size < 0 {
		filesJSONError(w, http.StatusBadRequest, "invalid_argument", "The size of the file is missing.")
		return
	}
	as := joinRel(dir, name)
	if strings.Contains(name, "/") || ops.CheckFilePath(name, false) != nil || ops.CheckFilePath(as, false) != nil {
		filesJSONError(w, http.StatusBadRequest, string(proto.CodePathOutsideRoot), "This file name cannot be used in the session's folder.")
		return
	}
	args := ops.FilePushArgs{Target: t.opTarget(), Path: name, As: as, Overwrite: r.PostFormValue("overwrite") == "1",
		Kind: ops.FileKindFile, Size: size, Entries: 1}
	var res ops.FileTransferResult
	if err := t.c.CallOp(r, ops.OpFilePush, args, &res); err != nil {
		filesOpError(w, err)
		return
	}
	a, ok := admissionOf(res, name)
	if !ok {
		filesJSONError(w, http.StatusBadGateway, "internal", "The Hub sent an admission the page cannot use.")
		return
	}
	a.Size = size
	writeFilesJSON(w, http.StatusOK, a)
}

// legServer serves a data-leg request in process: the Hub's root handler,
// where the files service is mounted at proto.FilesLegPathPrefix.
type legServer interface {
	serveLeg(w http.ResponseWriter, r *http.Request)
}

func (c *HandlerCaller) serveLeg(w http.ResponseWriter, r *http.Request) { c.h.ServeHTTP(w, r) }

// uploadBody is the body of the in-process upload request: the browser's
// body, hashed, with a read deadline that moves on while bytes arrive. At the
// end of a body of exactly the declared size it sets the three trailers of a
// single-file push; any other end leaves them empty.
type uploadBody struct {
	src     io.Reader
	rc      *http.ResponseController
	sum     hash.Hash
	n, size int64
	name    string
	trailer http.Header
	moved   time.Time
	now     func() time.Time
}

func (b *uploadBody) Read(p []byte) (int, error) {
	if now := b.now(); now.Sub(b.moved) > time.Second {
		b.moved = now
		_ = b.rc.SetReadDeadline(now.Add(uploadIdle))
	}
	n, err := b.src.Read(p)
	b.n += int64(n)
	_, _ = b.sum.Write(p[:n])
	if b.n > b.size {
		return n, errors.New("web: the upload is longer than its declared size")
	}
	if errors.Is(err, io.EOF) {
		if b.n != b.size {
			return n, io.ErrUnexpectedEOF
		}
		var sum [32]byte
		copy(sum[:], b.sum.Sum(nil))
		md, merr := filexfer.SingleFileManifest(b.name, b.size, sum).Digest()
		if merr != nil {
			return n, merr
		}
		field := proto.FormatDigestField(sum[:])
		b.trailer.Set(proto.HeaderContentDigest, field)
		b.trailer.Set(proto.HeaderReprDigest, field)
		b.trailer.Set(proto.HeaderManifestDigest, proto.FormatDigestField(md[:]))
	}
	return n, err
}

// handleFilesUpload streams the body of one admitted upload into the Hub's
// data-leg endpoint and answers its answer (201 with the receipt, or the
// refusal).
func handleFilesUpload(w http.ResponseWriter, r *http.Request, t target) {
	id := r.PathValue("transfer")
	token := r.Header.Get(proto.HeaderLegToken)
	name, nerr := url.PathUnescape(r.Header.Get(HeaderUploadName))
	if !ops.ValidUUIDv7(id) || !proto.ValidLegToken(token) || nerr != nil || ops.CheckFilePath(name, false) != nil || strings.Contains(name, "/") {
		filesJSONError(w, http.StatusBadRequest, "invalid_argument", "The upload is not one the page admitted.")
		return
	}
	if r.ContentLength < 0 {
		filesJSONError(w, http.StatusLengthRequired, "invalid_argument", "The upload must declare its length.")
		return
	}
	ls, ok := t.c.(legServer)
	sess, sok := session.FromContext(r.Context())
	if !ok || !sok {
		filesJSONError(w, http.StatusServiceUnavailable, "unavailable", "Uploads are not connected to the Hub yet.")
		return
	}
	trailer := http.Header{}
	for _, k := range []string{proto.HeaderContentDigest, proto.HeaderReprDigest, proto.HeaderManifestDigest} {
		trailer[http.CanonicalHeaderKey(k)] = nil
	}
	body := &uploadBody{src: r.Body, rc: http.NewResponseController(w), sum: sha256.New(), size: r.ContentLength,
		name: name, trailer: trailer, now: time.Now}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, proto.FilesLegPath(id), io.NopCloser(body))
	if err != nil {
		filesJSONError(w, http.StatusInternalServerError, "internal", "The upload could not be started.")
		return
	}
	req.ContentLength = -1
	req.Trailer = trailer
	req.Header.Set("Content-Type", proto.ContentTypeFile)
	req.Header.Set(proto.HeaderLegToken, token)
	req.Header.Set(session.CSRFHeader, sess.CSRFToken)
	if v := r.Header.Get("Cookie"); v != "" {
		req.Header.Set("Cookie", v)
	}
	for _, h := range forwardedHeaders {
		for _, v := range r.Header.Values(h) {
			req.Header.Add(h, v)
		}
	}
	req.Host, req.RemoteAddr, req.TLS = r.Host, r.RemoteAddr, r.TLS
	ls.serveLeg(w, req)
}

func init() {
	RegisterSessionPane(SessionPane{ID: "files", Label: "Files", Order: filesPaneOrder, Render: renderSessionFiles})
	RegisterRoute(Route{Pattern: "POST " + pathSessionPattern + pathFilesList, Handler: filesRoute(false, handleFilesList)})
	RegisterRoute(Route{Pattern: "POST " + pathSessionPattern + pathFilesPull, Handler: filesRoute(false, handleFilesPull)})
	RegisterRoute(Route{Pattern: "POST " + pathSessionPattern + pathFilesPush, Handler: filesRoute(false, handleFilesPush)})
	RegisterRoute(Route{Pattern: "PUT " + pathSessionPattern + pathFilesUpload, Handler: filesRoute(true, handleFilesUpload)})
}
