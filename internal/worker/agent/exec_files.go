package agent

// File transfer executor (files.v1, BRD minion-file-transfer v2, WP5): the
// target end of file_list, file_pull and file_push.
//
// A Hub request for a pull or a push is an admission: the Worker checks
// everything it enforces itself whatever the Hub says (SEC-3: local switch,
// preview_only, share selection, root floor and identity, protected and
// sensitive lists, entry types, limits), pins the root of the session's
// folder (filexfer.OpenPaneRoot), inspects the source or the destination and
// records a prepared transfer bound to the Hub's transfer id, the target leg
// token, the direction and the connection (Link) the request came on. The
// bytes then move on one data leg (files_http.go) that the Hub opens on the
// same connection within proto.LegOpenWindow; the leg consumes the prepared
// transfer, so a token is used once.
//
// Nothing here builds an absolute path from a wire path: every access is
// relative to the pinned root handle.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// ConfineLabelAndroid is the folder of the Android app Worker that holds
// every transferred file (BRD 5.4), and its display name.
const ConfineLabelAndroid = "Minion files"

// FilesConfig configures the file transfer end of a Worker ([NewFiles]).
type FilesConfig struct {
	// Mode returns the local switch file_transfer (ops.FileMode*). It is
	// asked at every request, when a leg opens and before every publish, so a
	// saved change applies at once. Nil is on.
	Mode func() string
	// Floor returns the identity floor the roots are checked against. It is
	// asked for every root, so a refreshed floor applies to the next request.
	// Nil means no floor, which only tests may use.
	Floor func() *filexfer.Floor
	// Confine and ConfineLabel are the Android rule (filexfer.RootOptions):
	// every root lies in Confine.
	Confine, ConfineLabel string
	// Home is the Worker user's home folder, for the staging log (the
	// absolute folders of staged files stay on the Worker).
	Home string
	// StateDir holds the staging log; "" keeps none.
	StateDir string
	// Unavailable, when not empty, is why this Worker cannot be a transfer end
	// (Windows, an unsupported kernel, a host without pane folders): every
	// file op answers files_root_unknown with it, and files.v1 is not
	// announced.
	Unavailable string
	// MinFreeBytes and MinFreePermille are the free-space reserve of a
	// receiver (0: the defaults, a negative MinFreeBytes disables the check;
	// tests).
	MinFreeBytes    int64
	MinFreePermille int64
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger receives operational messages: never paths, names, tokens or
	// content (NFR-9). Nil discards them.
	Logger *slog.Logger
}

// Files is the file transfer end of a Worker: the executor of the three file
// ops and the table of prepared transfers that the data legs consume. It is
// safe for concurrent use.
type Files struct {
	cfg    FilesConfig
	ex     *Executor
	opener term.PaneDirOpener
	log    *slog.Logger
	key    [32]byte // HMAC key of the list cursors (per process)

	mu       sync.Mutex
	prepared map[string]*fxTransfer
	// done holds the transfer ids whose leg token was used or expired, until
	// the end of their window, so that a second use is transfer_expired.
	done   map[string]time.Time
	active int // legs running
}

// fxTransfer is one prepared transfer.
type fxTransfer struct {
	id        string
	token     string
	direction string // ops.FileDirectionPull (this end sends) or push
	conn      any    // the connection that admitted it (*Link)
	expires   time.Time
	timer     *time.Timer

	root     *filexfer.Root
	rootPath string // the opened folder, for the staging log only
	path     string // pull: the source; push: the destination (wire path)
	srcName  string // the manifest name of a single file (last element of the source path)
	kind     string
	size     int64
	entries  int64
	etag     string
	src      *filexfer.SourceFile // pull of a single file
	limits   ops.FileTransferLimits
	overwrit bool
}

// close releases the handles of a transfer.
func (t *fxTransfer) close() {
	if t.src != nil {
		_ = t.src.Close()
	}
	if t.root != nil {
		_ = t.root.Close()
	}
}

// NewFiles returns the file transfer end of ex. The host's pane folder port
// (term.PaneDirOf) is required for a working end; without it the end answers
// files_root_unknown.
func NewFiles(ex *Executor, cfg FilesConfig) *Files {
	f := &Files{cfg: cfg, ex: ex, log: cfg.Logger, prepared: map[string]*fxTransfer{}, done: map[string]time.Time{}}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	if ex != nil {
		f.opener, _ = term.PaneDirOf(ex.host)
	}
	if _, err := rand.Read(f.key[:]); err != nil {
		// No randomness: cursors would be forgeable, so no listing pages.
		f.cfg.Unavailable = "no random source for the list cursors"
	}
	return f
}

// AttachFiles registers the file ops of f with the executor. The ops are
// announced to the Hub only when f announces files.v1 ([Executor.Ops],
// [Executor.Features]).
func (e *Executor) AttachFiles(f *Files) {
	e.files.Store(f)
	e.Handle(ops.OpFileList, f.fileList)
	e.Handle(ops.OpFilePull, f.filePull)
	e.Handle(ops.OpFilePush, f.filePush)
}

// Files returns the attached file transfer end, or nil.
func (e *Executor) Files() *Files { return e.files.Load() }

// filesAnnounced reports whether the executor announces files.v1.
func (e *Executor) filesAnnounced() bool {
	f := e.files.Load()
	return f != nil && f.Announce()
}

// Announce reports whether this Worker announces proto.FeatureFilesV1: the
// probe found a safe resolution, the host reports pane folders and the
// platform is not Windows (v1 has no Windows transfer end).
func (f *Files) Announce() bool { return f.unavailable() == "" }

// Status is the reason files.v1 is not announced, "" when it is.
func (f *Files) Status() string { return f.unavailable() }

func (f *Files) unavailable() string {
	switch {
	case runtime.GOOS == "windows":
		return "File transfer is not available on Windows Workers in this version"
	case f.cfg.Unavailable != "":
		return f.cfg.Unavailable
	case f.opener == nil:
		return "the terminal host cannot report the folder of a session"
	}
	return ""
}

func (f *Files) now() time.Time {
	if f.cfg.Now != nil {
		return f.cfg.Now()
	}
	return time.Now()
}

// Mode returns the local switch in force.
func (f *Files) Mode() string {
	if f.cfg.Mode == nil {
		return ops.FileModeOn
	}
	m := f.cfg.Mode()
	if !ops.ValidWorkerFileMode(m) {
		return ops.FileModeOff
	}
	return m
}

// requireAvailable answers files_root_unknown on an end that cannot serve.
func (f *Files) requireAvailable() error {
	if why := f.unavailable(); why != "" {
		return apiErr(proto.CodeFilesRootUnknown, "", why)
	}
	return nil
}

// requireEffect answers files_disabled when the local switch refuses effect
// (ops.FileEffectSend or ops.FileEffectReceive) on this end, and
// forbidden/preview_only for a receive on a preview-only Worker.
func (f *Files) requireEffect(effect string) error {
	if !ops.WorkerFileModeAllows(f.Mode(), effect) {
		what := "sending files"
		if effect == ops.FileEffectReceive {
			what = "receiving files"
		}
		return apiErr(proto.CodeFilesDisabled, "", "this Worker's file_transfer setting does not allow "+what)
	}
	if effect == ops.FileEffectReceive && f.ex != nil && f.ex.policy.PreviewOnly() {
		return apiErr(proto.CodeForbidden, proto.CodePreviewOnly, "the Worker is preview-only")
	}
	return nil
}

// fxErr maps an error of the transfer engine to an API error with its code.
// The message never names a path.
func fxErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := AsAPIError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	pe := filexfer.ProtoError(err)
	return &APIError{Code: pe.Code, Message: pe.Message, Err: err}
}

// rootOptions returns the options of a root opener now.
func (f *Files) rootOptions() filexfer.RootOptions {
	o := filexfer.RootOptions{Confine: f.cfg.Confine, ConfineLabel: f.cfg.ConfineLabel}
	if f.cfg.Floor != nil {
		o.Floor = f.cfg.Floor()
	}
	return o
}

// openRoot resolves the target session to its pane and pins the pane's folder
// (BRD FR-2, FR-3). The caller checks the share selection.
func (f *Files) openRoot(ctx context.Context, t ops.Target) (*filexfer.Root, string, error) {
	ref, err := f.ex.resolve(ctx, t)
	if err != nil {
		return nil, "", err
	}
	if f.opener == nil {
		return nil, "", apiErr(proto.CodeFilesRootUnknown, "", "the terminal host cannot report the folder of a session")
	}
	pd, err := f.opener.PaneDir(ctx, ref)
	switch {
	case errors.Is(err, term.ErrNotFound):
		return nil, "", hostErr("read the folder of the pane", err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, "", err
	case err != nil:
		return nil, "", &APIError{Code: proto.CodeFilesRootUnknown, Message: "the folder of the session cannot be proven", Err: err}
	}
	root, err := filexfer.OpenPaneRoot(pd.PID, pd.Path, f.rootOptions())
	if err != nil {
		return nil, "", fxErr(err)
	}
	return root, pd.Path, nil
}

// limitsOf returns the effective limits of a transfer: the files.v1 defaults
// lowered by the Hub's.
func limitsOf(leg ops.FileLegArgs) ops.FileTransferLimits {
	return ops.DefaultFileTransferLimits().Lower(ops.FileTransferLimits{MaxBytes: leg.MaxBytes, MaxEntries: leg.MaxEntries})
}

// requireLeg refuses a pull or push that did not come from the Hub's
// admission: without a transfer id, a leg token and a connection there is no
// data leg to prepare.
func requireLeg(ctx context.Context, leg ops.FileLegArgs) (*Link, error) {
	if leg.TransferID == "" || leg.LegToken == "" {
		return nil, apiErr(proto.CodeInvalidArgument, "", "a file transfer reaches the Worker only through the Hub's admission (transfer_id and leg_token)")
	}
	l := linkFrom(ctx)
	if l == nil {
		return nil, apiErr(proto.CodeInvalidArgument, "", "a file transfer needs the Hub connection that admitted it")
	}
	return l, nil
}

// ---- file_list ----

// listOverhead is the encoded size reserved for everything of a listing page
// but its entries: the root, the path, the cursor and the JSON frame.
const listOverhead = 64 + proto.MaxPathBytes*2 + proto.MaxFileListCursor*2

func (f *Files) fileList(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.FileListArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := f.ex.requireTargetWorker(ctx, args.Target.WorkerID); err != nil {
		return nil, err
	}
	if err := f.requireAvailable(); err != nil {
		return nil, err
	}
	if f.Mode() == ops.FileModeOff {
		return nil, apiErr(proto.CodeFilesDisabled, "", "this Worker's file_transfer setting is off")
	}
	if err := filexfer.CheckPath(args.Path, true); err != nil {
		return nil, fxErr(err)
	}
	if args.Path != "" && filexfer.SensitivePath(strings.Split(args.Path, "/")) {
		return nil, apiErr(proto.CodePathSensitive, "", "the folder is sensitive and is not listed")
	}
	return readStable(f.ex, ctx, args.Target.SessionName, func(store.ShareState) (ops.FileListResult, error) {
		root, _, err := f.openRoot(ctx, args.Target)
		if err != nil {
			return ops.FileListResult{}, err
		}
		defer func() { _ = root.Close() }()
		return f.list(root, args)
	})
}

// list reads one page of the folder args.Path beneath root.
func (f *Files) list(root *filexfer.Root, args ops.FileListArgs) (ops.FileListResult, error) {
	res := ops.FileListResult{Root: root.Display(), Path: args.Path}
	after, err := f.parseCursor(root, args.Path, args.Cursor)
	if err != nil {
		return res, err
	}
	dir, err := root.OpenDir(args.Path)
	if err != nil {
		return res, fxErr(err)
	}
	defer func() { _ = dir.Close() }()
	if st, err := dir.Stat(); err == nil {
		if id, ok := identityOf(st); ok && args.Path != "" && root.Floor() != nil && root.Floor().SensitiveDir(id) {
			return res, apiErr(proto.CodePathSensitive, "", "the folder is sensitive and is not listed")
		}
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return res, internalErr("read the folder", err)
	}
	sort.Strings(names)
	limit := args.EffectiveLimit()
	budget := proto.MaxFileControlResult - listOverhead
	rootDev := root.Identity().Dev
	floor := root.Floor()
	for _, name := range names {
		if args.Cursor != "" && name <= after {
			continue
		}
		e, ok := listEntry(dir, args.Path, name, rootDev, floor)
		if !ok {
			continue // gone since the read of the names
		}
		b, _ := json.Marshal(e)
		if len(res.Entries) >= limit || budget-len(b)-1 < 0 {
			if len(res.Entries) > 0 {
				res.NextCursor = f.cursor(root, args.Path, res.Entries[len(res.Entries)-1].Name)
			}
			break
		}
		budget -= len(b) + 1
		res.Entries = append(res.Entries, e)
	}
	return res, nil
}

// listEntry describes the entry name of dir (the folder rel beneath the root).
func listEntry(dir *os.File, rel, name string, rootDev uint64, floor *filexfer.Floor) (ops.FileEntry, bool) {
	st, err := lstatAt(dir, name)
	if err != nil {
		return ops.FileEntry{}, false
	}
	e := ops.FileEntry{Name: name, MTime: st.mtime.Unix()}
	elems := strings.Split(filexfer.JoinPath(rel, name), "/")
	switch {
	case st.isSymlink():
		e.Type, e.SkippedReason = proto.FileTypeSymlink, proto.SkipSymlink
	case st.isDir():
		e.Type = proto.FileTypeDir
		switch {
		case st.dev != rootDev:
			e.SkippedReason = proto.SkipMount
		case filexfer.SensitivePath(elems), floor != nil && floor.SensitiveDir(filexfer.Identity{Dev: st.dev, Ino: st.ino}):
			e.SkippedReason = proto.SkipSensitive
		}
	case st.isReg():
		e.Type, e.Size = proto.FileTypeFile, st.size
		switch {
		case st.nlink != 1:
			e.SkippedReason = proto.SkipHardlink
		case st.dev != rootDev:
			e.SkippedReason = proto.SkipMount
		case filexfer.SensitivePath(elems):
			e.SkippedReason = proto.SkipSensitive
		}
	default:
		e.Type, e.SkippedReason = proto.FileTypeOther, proto.SkipSpecial
	}
	if e.SkippedReason == "" && filexfer.CheckPath(filexfer.JoinPath(rel, name), false) != nil {
		e.SkippedReason = proto.SkipName
	}
	if !utf8.ValidString(name) {
		// A name that is not UTF-8 cannot be put in JSON faithfully: it is
		// listed as unreadable, never as a transferable entry.
		e.Name = strings.ToValidUTF8(name, "\uFFFD")
		e.SkippedReason = proto.SkipName
	}
	return e, true
}

// cursorMACBytes is the size of the MAC of a list cursor.
const cursorMACBytes = 16

// cursorMAC binds a cursor to the root identity, the listed folder and the
// last name.
func (f *Files) cursorMAC(root *filexfer.Root, rel, name string) []byte {
	m := hmac.New(sha256.New, f.key[:])
	var b [16]byte
	id := root.Identity()
	binary.BigEndian.PutUint64(b[:8], id.Dev)
	binary.BigEndian.PutUint64(b[8:], id.Ino)
	m.Write(b[:])
	m.Write([]byte(rel))
	m.Write([]byte{0})
	m.Write([]byte(name))
	return m.Sum(nil)[:cursorMACBytes]
}

// cursor returns the next_cursor after name.
func (f *Files) cursor(root *filexfer.Root, rel, name string) string {
	return base64.RawURLEncoding.EncodeToString(append(f.cursorMAC(root, rel, name), name...))
}

// parseCursor returns the last name of the previous page; "" starts at the
// beginning. A cursor of another folder, root or process is invalid_argument.
func (f *Files) parseCursor(root *filexfer.Root, rel, c string) (string, error) {
	if c == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) <= cursorMACBytes {
		return "", apiErr(proto.CodeInvalidArgument, "", "the cursor is not a cursor of this folder")
	}
	name := string(b[cursorMACBytes:])
	if !hmac.Equal(b[:cursorMACBytes], f.cursorMAC(root, rel, name)) {
		return "", apiErr(proto.CodeInvalidArgument, "", "the cursor is not a cursor of this folder")
	}
	return name, nil
}

// ---- file_pull (this end sends) ----

func (f *Files) filePull(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.FilePullArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := f.ex.requireTargetWorker(ctx, args.Target.WorkerID); err != nil {
		return nil, err
	}
	if err := f.requireAvailable(); err != nil {
		return nil, err
	}
	conn, err := requireLeg(ctx, args.FileLegArgs)
	if err != nil {
		return nil, err
	}
	if err := f.requireEffect(ops.FileEffectSend); err != nil {
		return nil, err
	}
	if err := filexfer.CheckPath(args.Path, false); err != nil {
		return nil, fxErr(err)
	}
	if err := filexfer.CheckPull(args.Path); err != nil {
		return nil, fxErr(err)
	}
	return f.admitStable(ctx, args.Target.SessionName, func() (*fxTransfer, error) {
		root, rootPath, err := f.openRoot(ctx, args.Target)
		if err != nil {
			return nil, err
		}
		t := &fxTransfer{
			id: args.TransferID, token: args.LegToken, direction: ops.FileDirectionPull, conn: conn,
			root: root, rootPath: rootPath, path: args.Path, srcName: filexfer.BaseName(args.Path),
			limits: limitsOf(args.FileLegArgs),
		}
		if err := f.inspectSource(ctx, t); err != nil {
			t.close()
			return nil, err
		}
		return t, nil
	})
}

// admitStable inspects a transfer with inspect under the share rule
// (readStable: an inspection the rule changed under is made again, and its
// handles are released) and then records it as prepared.
func (f *Files) admitStable(ctx context.Context, session string, inspect func() (*fxTransfer, error)) (ops.FileTransferResult, error) {
	var made []*fxTransfer
	t, err := readStable(f.ex, ctx, session, func(store.ShareState) (*fxTransfer, error) {
		t, err := inspect()
		if t != nil {
			made = append(made, t)
		}
		return t, err
	})
	for _, m := range made {
		if m != t || err != nil {
			m.close()
		}
	}
	if err != nil {
		return ops.FileTransferResult{}, err
	}
	res, err := f.admit(t)
	if err != nil {
		t.close()
	}
	return res, err
}

// inspectSource opens the source of a pull and fills kind, size, entries and
// the ETag of a stable single file.
func (f *Files) inspectSource(ctx context.Context, t *fxTransfer) error {
	h, err := t.root.OpenFile(t.path)
	if err != nil {
		return fxErr(err)
	}
	st, err := h.Stat()
	_ = h.Close()
	if err != nil {
		return internalErr("read the source", err)
	}
	switch {
	case st.IsDir():
		if id, ok := identityOf(st); ok && t.root.Floor() != nil && t.root.Floor().SensitiveDir(id) {
			return apiErr(proto.CodePathSensitive, "", "the folder is sensitive and cannot be sent")
		}
		lim := filexfer.LimitsFrom(t.limits)
		stats, err := filexfer.Walk(ctx, t.root, t.path, filexfer.WalkOptions{Limits: lim, Filter: filexfer.PullFilter(t.root, nil)}, false,
			func(filexfer.Entry, *os.File) error { return nil })
		if err != nil {
			return fxErr(err)
		}
		t.kind, t.size, t.entries = ops.FileKindDir, stats.Bytes, stats.Files+stats.Dirs
	case st.Mode().IsRegular():
		src, err := filexfer.OpenSourceFile(t.root, t.path)
		if err != nil {
			return fxErr(err)
		}
		t.src = src
		t.kind, t.size, t.entries = ops.FileKindFile, src.Size, 1
		if src.Size > t.limits.MaxBytes {
			return apiErr(proto.CodeTransferTooLarge, "", "the file is larger than the transfer limit")
		}
		t.etag = f.etagOf(t.root, src)
	default:
		return apiErr(proto.CodeFileTypeRefused, "", "the entry is not a regular file or folder")
	}
	return nil
}

// etagOf returns the strong ETag of a stable file (BRD 5.7): its ctime and
// mtime at least proto.StableFileAge old. An unstable file gets none and is
// sent without ranges.
func (f *Files) etagOf(root *filexfer.Root, src *filexfer.SourceFile) string {
	now := f.now()
	if now.Sub(src.MTime) < proto.StableFileAge || now.Sub(src.CTime) < proto.StableFileAge {
		return ""
	}
	id := root.Identity()
	var b [56]byte
	binary.BigEndian.PutUint64(b[0:], src.Dev)
	binary.BigEndian.PutUint64(b[8:], src.Ino)
	binary.BigEndian.PutUint64(b[16:], uint64(src.Size))             //nolint:gosec // a size is never negative
	binary.BigEndian.PutUint64(b[24:], uint64(src.MTime.UnixNano())) //nolint:gosec // bits only
	binary.BigEndian.PutUint64(b[32:], uint64(src.CTime.UnixNano())) //nolint:gosec // bits only
	binary.BigEndian.PutUint64(b[40:], id.Dev)
	binary.BigEndian.PutUint64(b[48:], id.Ino)
	sum := sha256.Sum256(b[:])
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`
}

// ---- file_push (this end receives) ----

func (f *Files) filePush(ctx context.Context, rq Request) (any, error) {
	_, args, err := prepare[ops.FilePushArgs](rq)
	if err != nil {
		return nil, err
	}
	if err := f.ex.requireTargetWorker(ctx, args.Target.WorkerID); err != nil {
		return nil, err
	}
	if err := f.requireAvailable(); err != nil {
		return nil, err
	}
	conn, err := requireLeg(ctx, args.FileLegArgs)
	if err != nil {
		return nil, err
	}
	if err := f.requireEffect(ops.FileEffectReceive); err != nil {
		return nil, err
	}
	dest := args.As
	if dest == "" {
		if err := filexfer.CheckPath(args.Path, false); err != nil {
			return nil, fxErr(err)
		}
		dest = filexfer.BaseName(args.Path)
	}
	if err := filexfer.CheckPath(dest, false); err != nil {
		return nil, fxErr(err)
	}
	if args.Kind == "" {
		return nil, apiErr(proto.CodeInvalidArgument, "", "a push declares the kind of its source (file or dir)")
	}
	lim := limitsOf(args.FileLegArgs)
	if args.Size > lim.MaxBytes || args.Entries > lim.MaxEntries {
		return nil, apiErr(proto.CodeTransferTooLarge, "", "the declared size or entry count is above the transfer limit")
	}
	return f.admitStable(ctx, args.Target.SessionName, func() (*fxTransfer, error) {
		root, rootPath, err := f.openRoot(ctx, args.Target)
		if err != nil {
			return nil, err
		}
		t := &fxTransfer{
			id: args.TransferID, token: args.LegToken, direction: ops.FileDirectionPush, conn: conn,
			root: root, rootPath: rootPath, path: dest, kind: args.Kind, size: args.Size, entries: args.Entries,
			srcName: filexfer.BaseName(args.Path), limits: lim, overwrit: args.Overwrite,
		}
		if t.srcName == "" || filexfer.CheckPath(args.Path, false) != nil {
			t.srcName = filexfer.BaseName(dest)
		}
		if err := f.inspectDest(t); err != nil {
			t.close()
			return nil, err
		}
		return t, nil
	})
}

// inspectDest checks the destination of a push at admission: protected
// names and folders, an existing name (conflict without overwrite) and the
// free space for the declared size. The stager checks everything again while
// it stages.
func (f *Files) inspectDest(t *fxTransfer) error {
	if err := t.root.CheckPush(); err != nil {
		return fxErr(err)
	}
	if err := filexfer.PushCheck(t.root, nil)(t.path, t.kind == ops.FileKindDir); err != nil {
		return fxErr(err)
	}
	parent, name, err := t.root.OpenParent(t.path)
	switch {
	case err == nil:
		st, serr := lstatAt(parent, name)
		_ = parent.Close()
		if serr == nil {
			switch {
			case t.kind == ops.FileKindDir && !st.isDir():
				return apiErr(proto.CodeConflict, "", "the destination exists and is not a folder")
			case t.kind == ops.FileKindFile && (!t.overwrit || !st.isReg() || st.nlink != 1):
				return apiErr(proto.CodeConflict, "", "the destination exists")
			}
		} else if !errors.Is(serr, fs.ErrNotExist) {
			return fxErr(serr)
		}
	case errors.Is(err, fs.ErrNotExist):
		// The folders are created at publish.
	default:
		return fxErr(err)
	}
	return f.checkFree(t.root, t.size)
}

// checkFree refuses a push whose declared size would leave less than the
// reserve on the receiving file system (NFR-6).
func (f *Files) checkFree(root *filexfer.Root, size int64) error {
	minBytes, permille := f.cfg.MinFreeBytes, f.cfg.MinFreePermille
	if minBytes < 0 {
		return nil
	}
	if minBytes == 0 {
		minBytes = proto.MinFreeBytes
	}
	if permille <= 0 {
		permille = proto.MinFreePermille
	}
	avail, total, err := freeSpace(root.Fd())
	if err != nil {
		return nil // the stager checks again per file
	}
	reserve := max(minBytes, total/1000*permille)
	if avail-size < reserve {
		return apiErr(proto.CodeTransferTooLarge, "", "the receiving file system has not enough free space")
	}
	return nil
}

// ---- prepared transfers ----

// admit records t as prepared and returns the admission result. A transfer
// id that is known already is conflict; more than proto.MaxTransfersPerWorker
// transfers at once is rate_limited.
func (f *Files) admit(t *fxTransfer) (ops.FileTransferResult, error) {
	now := f.now()
	t.expires = now.Add(proto.LegOpenWindow)
	f.mu.Lock()
	f.sweepLocked(now)
	if _, dup := f.prepared[t.id]; dup {
		f.mu.Unlock()
		return ops.FileTransferResult{}, apiErr(proto.CodeConflict, "", "the transfer was admitted already")
	}
	if _, used := f.done[t.id]; used {
		f.mu.Unlock()
		return ops.FileTransferResult{}, apiErr(proto.CodeConflict, "", "the transfer was admitted already")
	}
	if len(f.prepared)+f.active >= proto.MaxTransfersPerWorker {
		f.mu.Unlock()
		return ops.FileTransferResult{}, apiErr(proto.CodeRateLimited, "", "the Worker runs as many file transfers as it may")
	}
	f.prepared[t.id] = t
	id := t.id
	t.timer = time.AfterFunc(proto.LegOpenWindow, func() { f.expire(id) })
	f.mu.Unlock()

	exp := ops.Timestamp(t.expires)
	lim := t.limits
	res := ops.FileTransferResult{
		Stage: ops.FileStageAdmitted, TransferID: t.id, Direction: t.direction,
		TargetRoot: t.root.Display(), Kind: t.kind, Limits: &lim, ExpiresAt: &exp,
		LegPath: proto.FilesLegPath(t.id),
	}
	if t.direction == ops.FileDirectionPull {
		res.Size, res.ETag = t.size, t.etag
		if t.kind == ops.FileKindDir {
			res.Encodings = []string{proto.EncodingZstd, proto.EncodingGzip, proto.EncodingIdentity}
		} else {
			res.Encodings = []string{proto.EncodingZstd, proto.EncodingIdentity}
		}
	} else {
		res.Size = t.size
		res.Encodings = []string{proto.EncodingZstd, proto.EncodingGzip, proto.EncodingIdentity}
	}
	f.log.Info("file transfer prepared", "transfer", t.id, "dir", t.direction, "kind", t.kind)
	return res, nil
}

// expire drops a prepared transfer whose leg did not open in time.
func (f *Files) expire(id string) {
	f.mu.Lock()
	t, ok := f.prepared[id]
	if ok {
		delete(f.prepared, id)
		f.done[id] = t.expires.Add(proto.LegOpenWindow)
	}
	f.mu.Unlock()
	if ok {
		t.close()
		f.log.Info("file transfer expired before its data leg", "transfer", id)
	}
}

// sweepLocked forgets used ids whose window is over.
func (f *Files) sweepLocked(now time.Time) {
	for id, until := range f.done {
		if now.After(until) {
			delete(f.done, id)
		}
	}
}

// legRefusal is why a data leg is refused.
type legRefusal struct {
	code proto.Code
	msg  string
}

// claim consumes the prepared transfer id for a leg with token, direction
// (ops.FileDirectionPull for a GET, push for a PUT) on conn. Any mismatch
// refuses the leg; the token is single use whatever happens next. A wrong
// connection, direction or token is forbidden (403); a used or expired token
// is transfer_expired (410).
func (f *Files) claim(id, token, direction string, conn any) (*fxTransfer, *legRefusal) {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweepLocked(now)
	t, ok := f.prepared[id]
	if !ok {
		if _, used := f.done[id]; used {
			return nil, &legRefusal{proto.CodeTransferExpired, "the data leg of this transfer was used or has expired"}
		}
		return nil, &legRefusal{proto.CodeForbidden, "no transfer is prepared for this data leg"}
	}
	if !proto.LegTokenEqual(token, t.token) || t.direction != direction || t.conn != conn {
		return nil, &legRefusal{proto.CodeForbidden, "the data leg does not match its transfer"}
	}
	delete(f.prepared, id)
	f.done[id] = t.expires.Add(proto.LegOpenWindow)
	if t.timer != nil {
		t.timer.Stop()
	}
	if now.After(t.expires) {
		t.close()
		return nil, &legRefusal{proto.CodeTransferExpired, "the data leg opened after the transfer expired"}
	}
	f.active++
	return t, nil
}

// release ends a claimed transfer.
func (f *Files) release(t *fxTransfer) {
	t.close()
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
}

// DropConn forgets the prepared transfers of a connection that ended: their
// legs can never open.
func (f *Files) DropConn(conn any) {
	f.mu.Lock()
	var gone []*fxTransfer
	for id, t := range f.prepared {
		if t.conn == conn {
			delete(f.prepared, id)
			f.done[id] = t.expires.Add(proto.LegOpenWindow)
			if t.timer != nil {
				t.timer.Stop()
			}
			gone = append(gone, t)
		}
	}
	f.mu.Unlock()
	for _, t := range gone {
		t.close()
	}
}

// stagingDir returns the absolute folder of a staging directory for the
// staging log, or "" when it cannot be told. It never leaves the Worker.
func (f *Files) stagingDir(t *fxTransfer, rel string) string {
	if t.rootPath == "" || !filepath.IsAbs(t.rootPath) {
		return ""
	}
	base := t.rootPath
	if f.cfg.Confine != "" {
		// The Android rule may have chosen the confine folder itself.
		disp := t.root.Display()
		switch {
		case disp == f.cfg.ConfineLabel:
			base = f.cfg.Confine
		case strings.HasPrefix(disp, f.cfg.ConfineLabel+"/"):
			base = filepath.Join(f.cfg.Confine, filepath.FromSlash(strings.TrimPrefix(disp, f.cfg.ConfineLabel+"/")))
		}
	}
	if rel == "" {
		return base
	}
	return filepath.Join(base, filepath.FromSlash(rel))
}
