//go:build linux || darwin

package filexfer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// noParts fails when a staging file is left anywhere below dir.
func noParts(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && strings.HasPrefix(info.Name(), proto.StagingPrefix) {
			t.Fatalf("staging file left: %s", p)
		}
		return nil
	})
}

// entriesBelow lists the names below dir (relative, slash separated).
func entriesBelow(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func stageOpts() StageOptions { return StageOptions{MinFreeBytes: -1} }

// buildTree creates a source tree with transferable and untransferable
// entries and returns the root folder.
func buildTree(t *testing.T) string {
	src := t.TempDir()
	write(t, filepath.Join(src, "proj/a.txt"), []byte("alpha\n"))
	write(t, filepath.Join(src, "proj/sub/b.bin"), randomBytes(300<<10))
	write(t, filepath.Join(src, "proj/sub/deep/c"), nil)
	write(t, filepath.Join(src, "proj/.ssh/id_ed25519"), []byte("secret"))
	write(t, filepath.Join(src, "proj/h1"), []byte("hard"))
	if err := os.Link(filepath.Join(src, "proj/h1"), filepath.Join(src, "proj/h2")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "proj/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "proj/passwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(src, "proj/sublink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(src, "proj/fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "proj/a.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	return src
}

func pullFilter(rootRel string, _ Entry) string {
	if SensitivePath(pathElems(rootRel)) {
		return proto.SkipSensitive
	}
	return ""
}

// sendDir packs base of src with enc and returns the body, the manifest
// digest and the walk result.
func sendDir(t *testing.T, src, base, enc string) ([]byte, [32]byte, PackResult) {
	t.Helper()
	var body bytes.Buffer
	ww := NewWireWriter(&body)
	zw, err := NewEncoder(ww, enc)
	if err != nil {
		t.Fatal(err)
	}
	res, err := PackDir(context.Background(), zw, newTestRoot(t, src), base, WalkOptions{Filter: pullFilter})
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	digest, err := res.Manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), digest, res
}

// receiveDir runs the receiver of a folder transfer up to and including
// Publish.
func receiveDir(t *testing.T, dst, dest, enc string, body []byte, digest []byte, opts StageOptions) (PublishResult, error) {
	t.Helper()
	st, err := NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), dest, ops.FileKindDir, opts)
	if err != nil {
		return PublishResult{}, err
	}
	defer func() { _ = st.Abort() }()
	dec, err := NewDecoder(NewWireReader(bytes.NewReader(body), 0), enc, opts.Limits)
	if err != nil {
		return PublishResult{}, err
	}
	defer func() { _ = dec.Close() }()
	m, err := Unpack(dec, st)
	if err != nil {
		return PublishResult{}, err
	}
	if err := st.Verify(m, digest); err != nil {
		return PublishResult{}, err
	}
	return st.Publish()
}

func TestFolderRoundTrip(t *testing.T) {
	for _, enc := range []string{proto.EncodingZstd, proto.EncodingGzip, proto.EncodingIdentity} {
		t.Run(enc, func(t *testing.T) {
			src := buildTree(t)
			body, digest, res := sendDir(t, src, "proj", enc)
			skips := map[string]string{}
			for _, s := range res.Stats.Skips {
				skips[s.Path] = s.Reason
			}
			want := map[string]string{
				".ssh": proto.SkipSensitive, "h1": proto.SkipHardlink, "h2": proto.SkipHardlink,
				"passwd": proto.SkipSymlink, "sublink": proto.SkipSymlink, "fifo": proto.SkipSpecial,
			}
			for p, r := range want {
				if skips[p] != r {
					t.Fatalf("skip of %s: got %q want %q (all %v)", p, skips[p], r, skips)
				}
			}
			dst := t.TempDir()
			pub, err := receiveDir(t, dst, "copy/of/proj", enc, body, digest[:], stageOpts())
			if err != nil {
				t.Fatal(err)
			}
			if pub.Files != 3 {
				t.Fatalf("published %d files", pub.Files)
			}
			got := entriesBelow(t, filepath.Join(dst, "copy/of/proj"))
			wantNames := []string{"a.txt", "empty", "sub", "sub/b.bin", "sub/deep", "sub/deep/c"}
			if strings.Join(got, ",") != strings.Join(wantNames, ",") {
				t.Fatalf("published %v", got)
			}
			for _, p := range []string{"a.txt", "sub/b.bin", "sub/deep/c"} {
				a, _ := os.ReadFile(filepath.Join(src, "proj", p))
				b, _ := os.ReadFile(filepath.Join(dst, "copy/of/proj", p))
				if !bytes.Equal(a, b) {
					t.Fatalf("%s differs", p)
				}
			}
			fi, _ := os.Stat(filepath.Join(dst, "copy/of/proj/a.txt"))
			wantT := time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)
			if runtime.GOOS == "darwin" {
				wantT = wantT.Truncate(time.Microsecond)
			}
			if !fi.ModTime().Equal(wantT) {
				t.Fatalf("mtime %v, want %v", fi.ModTime(), wantT)
			}
			if fi.Mode().Perm() != os.FileMode(0o644&^umaskOnce()) {
				t.Fatalf("mode %v", fi.Mode())
			}
			noParts(t, dst)
		})
	}
}

// archive is a crafted tar stream for the restricted-profile tests.
type archive struct {
	t   *testing.T
	buf bytes.Buffer
	tw  *tar.Writer
	m   Manifest
}

func newArchive(t *testing.T) *archive {
	a := &archive{t: t}
	a.tw = tar.NewWriter(&a.buf)
	return a
}

func (a *archive) header(h *tar.Header, data []byte) {
	a.t.Helper()
	if err := a.tw.WriteHeader(h); err != nil {
		a.t.Fatal(err)
	}
	if len(data) > 0 {
		if _, err := a.tw.Write(data); err != nil {
			a.t.Fatal(err)
		}
	}
}

func (a *archive) dir(name string) {
	a.header(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, Format: tar.FormatPAX}, nil)
	_ = a.m.Add(proto.ManifestEntry{Path: name, Type: proto.FileTypeDir}, 0)
}

func (a *archive) file(name string, data []byte, mode int64) {
	a.header(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(data)), Mode: mode, Format: tar.FormatPAX,
		ModTime: time.Unix(1600000000, 0)}, data)
	sum := sha256.Sum256(data)
	_ = a.m.Add(proto.ManifestEntry{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Type: proto.FileTypeFile}, 0)
}

func (a *archive) manifest() {
	b, _ := a.m.Canonical()
	a.header(&tar.Header{Typeflag: tar.TypeReg, Name: proto.ManifestName, Size: int64(len(b)), Mode: 0o644, Format: tar.FormatPAX}, b)
}

func (a *archive) bytes() []byte {
	if err := a.tw.Close(); err != nil {
		a.t.Fatal(err)
	}
	return a.buf.Bytes()
}

func (a *archive) digest() []byte {
	d, _ := a.m.Digest()
	return d[:]
}

// rejected runs a crafted archive through the receiver and requires the
// code; nothing may be published and no staging file may stay.
func rejected(t *testing.T, name string, body, digest []byte, code proto.Code) {
	t.Helper()
	dst := t.TempDir()
	_, err := receiveDir(t, dst, "out", proto.EncodingIdentity, body, digest, stageOpts())
	if proto.CodeOf(err) != code {
		t.Fatalf("%s: got %v, want %s", name, err, code)
	}
	if got := entriesBelow(t, dst); len(got) != 0 {
		t.Fatalf("%s: published %v", name, got)
	}
}

func TestRestrictedProfile(t *testing.T) {
	type mk func(a *archive)
	cases := []struct {
		name string
		mk   mk
		code proto.Code
	}{
		{"symlink entry", func(a *archive) {
			a.header(&tar.Header{Typeflag: tar.TypeSymlink, Name: "l", Linkname: "/etc/passwd", Format: tar.FormatPAX}, nil)
		}, proto.CodeFileTypeRefused},
		{"hardlink entry", func(a *archive) {
			a.file("f", []byte("x"), 0o644)
			a.header(&tar.Header{Typeflag: tar.TypeLink, Name: "h", Linkname: "f", Format: tar.FormatPAX}, nil)
		}, proto.CodeFileTypeRefused},
		{"device entry", func(a *archive) {
			a.header(&tar.Header{Typeflag: tar.TypeChar, Name: "null", Devmajor: 1, Devminor: 3, Format: tar.FormatPAX}, nil)
		}, proto.CodeFileTypeRefused},
		{"fifo entry", func(a *archive) {
			a.header(&tar.Header{Typeflag: tar.TypeFifo, Name: "p", Format: tar.FormatPAX}, nil)
		}, proto.CodeFileTypeRefused},
		{"linkpath record", func(a *archive) {
			// A link name longer than the ustar field is a pax linkpath record.
			a.header(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Format: tar.FormatPAX, Linkname: strings.Repeat("x", 200)}, nil)
		}, proto.CodeFileTypeRefused},
		{"xattr record", func(a *archive) {
			a.header(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Format: tar.FormatPAX,
				PAXRecords: map[string]string{"SCHILY.xattr.user.x": "y"}}, nil)
		}, proto.CodeIntegrityFailed},
		{"owner names", func(a *archive) {
			a.header(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Uname: "root", Format: tar.FormatPAX}, nil)
		}, proto.CodeIntegrityFailed},
		{"GNU long name", func(a *archive) {
			a.header(&tar.Header{Typeflag: tar.TypeReg, Name: strings.Repeat("n", 150), Format: tar.FormatGNU}, nil)
		}, proto.CodeIntegrityFailed},
		{"oversized pax header", func(a *archive) {
			a.header(&tar.Header{Typeflag: tar.TypeReg, Name: strings.Repeat("d/", 3000) + "f", Format: tar.FormatPAX}, nil)
		}, proto.CodeTransferTooLarge},
		{"hostile path", func(a *archive) { a.file("../evil", []byte("x"), 0o644); a.manifest() }, proto.CodePathOutsideRoot},
		{"absolute path", func(a *archive) { a.file("/etc/evil", []byte("x"), 0o644); a.manifest() }, proto.CodePathOutsideRoot},
		{"staging name", func(a *archive) { a.file(".minion-part-x", []byte("x"), 0o644); a.manifest() }, proto.CodePathOutsideRoot},
		{"entry before its folder", func(a *archive) { a.file("d/f", []byte("x"), 0o644); a.manifest() }, proto.CodeIntegrityFailed},
		{"duplicate path", func(a *archive) {
			a.file("f", []byte("x"), 0o644)
			a.file("f", []byte("y"), 0o644)
			a.manifest()
		}, proto.CodeIntegrityFailed},
		{"no manifest", func(a *archive) { a.file("f", []byte("x"), 0o644) }, proto.CodeIntegrityFailed},
		{"entry after manifest", func(a *archive) {
			a.file("f", []byte("x"), 0o644)
			a.manifest()
			a.header(&tar.Header{Typeflag: tar.TypeReg, Name: "g", Format: tar.FormatPAX}, nil)
		}, proto.CodeIntegrityFailed},
		{"manifest mismatch", func(a *archive) {
			a.file("f", []byte("x"), 0o644)
			a.m.Entries[0].Size = 2
			a.manifest()
		}, proto.CodeIntegrityFailed},
		{"extra entry", func(a *archive) {
			a.file("f", []byte("x"), 0o644)
			b, _ := a.m.Canonical()
			a.file("g", []byte("y"), 0o644)
			a.header(&tar.Header{Typeflag: tar.TypeReg, Name: proto.ManifestName, Size: int64(len(b)), Format: tar.FormatPAX}, b)
		}, proto.CodeIntegrityFailed},
	}
	for _, c := range cases {
		a := newArchive(t)
		c.mk(a)
		rejected(t, c.name, a.bytes(), a.digest(), c.code)
	}
}

func TestArchiveTermination(t *testing.T) {
	a := newArchive(t)
	a.dir("d")
	a.file("d/f", []byte("content"), 0o644)
	a.manifest()
	good := a.bytes()
	digest := a.digest()
	// The good archive publishes.
	dst := t.TempDir()
	if _, err := receiveDir(t, dst, "out", proto.EncodingIdentity, good, digest, stageOpts()); err != nil {
		t.Fatal(err)
	}
	end := len(good) - 1024 // the two zero blocks
	cases := map[string][]byte{
		"no end marker":      good[:end],
		"one zero block":     good[:end+512],
		"trailing byte":      append(append([]byte{}, good...), 0),
		"trailing block":     append(append([]byte{}, good...), make([]byte, 512)...),
		"truncated manifest": good[:end-600],
		"truncated content":  good[:1500],
		"bit flip in content": func() []byte {
			b := append([]byte{}, good...)
			i := bytes.Index(b, []byte("content"))
			b[i] ^= 1
			return b
		}(),
	}
	for name, body := range cases {
		rejected(t, name, body, digest, proto.CodeIntegrityFailed)
	}
	// A wrong or missing manifest digest (trailer).
	rejected(t, "wrong digest", good, make([]byte, 32), proto.CodeIntegrityFailed)
	rejected(t, "missing digest", good, nil, proto.CodeIntegrityFailed)
}

// A bit flip on the coded wire (relay) leaves nothing published.
func TestWireBitFlip(t *testing.T) {
	src := buildTree(t)
	body, digest, _ := sendDir(t, src, "proj", proto.EncodingZstd)
	for _, i := range []int{20, len(body) / 2, len(body) - 2} {
		bad := flip(body, i)
		dst := t.TempDir()
		_, err := receiveDir(t, dst, "out", proto.EncodingZstd, bad, digest[:], stageOpts())
		if err == nil {
			t.Fatalf("flip at %d accepted", i)
		}
		if got := entriesBelow(t, dst); len(got) != 0 {
			t.Fatalf("flip at %d published %v", i, got)
		}
	}
}

func TestSetuidModeIsDropped(t *testing.T) {
	a := newArchive(t)
	a.file("tool", []byte("#!/bin/sh\n"), 0o4755)
	a.manifest()
	dst := t.TempDir()
	if _, err := receiveDir(t, dst, "out", proto.EncodingIdentity, a.bytes(), a.digest(), stageOpts()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "out/tool"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != os.FileMode(0o644&^umaskOnce()) {
		t.Fatalf("mode %v", fi.Mode())
	}
	if !fi.ModTime().Equal(time.Unix(1600000000, 0)) {
		t.Fatalf("mtime %v", fi.ModTime())
	}
}

// stageSingle stages and verifies a single file to dest and returns the
// stager.
func stageSingle(t *testing.T, dst, dest string, data []byte, opts StageOptions) *Stager {
	t.Helper()
	st, err := NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), dest, ops.FileKindFile, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Abort() })
	sum, err := ReceiveFile(bytes.NewReader(data), st, int64(len(data)), time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := SingleFileManifest("src.txt", int64(len(data)), sum).Digest()
	if err := st.Verify(SingleFileManifest("src.txt", int64(len(data)), sum), d[:]); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestNoReplacePublishRace(t *testing.T) {
	modes := []bool{false}
	if haveLinkFallback {
		modes = append(modes, true) // the link+unlink fallback
	}
	for _, fallback := range modes {
		setLinkFallback(fallback)
		// Deterministic: a creator wins between the check and the rename.
		dst := t.TempDir()
		st := stageSingle(t, dst, "x.txt", []byte("ours"), stageOpts())
		beforeRenameHook = func(_ *os.File, name string) {
			write(t, filepath.Join(dst, name), []byte("theirs"))
		}
		_, err := st.Publish()
		beforeRenameHook = nil
		if proto.CodeOf(err) != proto.CodeConflict {
			t.Fatalf("fallback=%v: got %v", fallback, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "x.txt")); string(b) != "theirs" {
			t.Fatalf("fallback=%v: the concurrent file was replaced: %q", fallback, b)
		}
		noParts(t, dst)
		// Stress: a racing O_EXCL creator; exactly one side wins and its
		// content stays.
		for i := 0; i < 50; i++ {
			dst := t.TempDir()
			st := stageSingle(t, dst, "y.txt", []byte("ours"), stageOpts())
			var wg sync.WaitGroup
			var theirs bool
			wg.Add(1)
			go func() {
				defer wg.Done()
				f, err := os.OpenFile(filepath.Join(dst, "y.txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
				if err == nil {
					_, _ = f.WriteString("theirs")
					_ = f.Close()
					theirs = true
				}
			}()
			_, err := st.Publish()
			wg.Wait()
			b, _ := os.ReadFile(filepath.Join(dst, "y.txt"))
			switch {
			case err == nil && !theirs && string(b) == "ours":
			case proto.CodeOf(err) == proto.CodeConflict && theirs && string(b) == "theirs":
			default:
				t.Fatalf("fallback=%v: publish %v, creator %v, content %q", fallback, err, theirs, b)
			}
			noParts(t, dst)
		}
	}
	setLinkFallback(false)
}

func TestOverwriteRules(t *testing.T) {
	dst := t.TempDir()
	write(t, filepath.Join(dst, "keep.txt"), []byte("old"))
	st := stageSingle(t, dst, "keep.txt", []byte("new"), stageOpts())
	if _, err := st.Publish(); proto.CodeOf(err) != proto.CodeConflict {
		t.Fatalf("without overwrite: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(b) != "old" {
		t.Fatal("the existing file was touched")
	}
	ow := stageOpts()
	ow.Overwrite = true
	st = stageSingle(t, dst, "keep.txt", []byte("new"), ow)
	if _, err := st.Publish(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(b) != "new" {
		t.Fatal("not replaced")
	}
	// A hard-linked file, a symlink and a directory are never replaced.
	write(t, filepath.Join(dst, "linked"), []byte("l"))
	_ = os.Link(filepath.Join(dst, "linked"), filepath.Join(dst, "linked2"))
	_ = os.Symlink("/etc/passwd", filepath.Join(dst, "sym"))
	_ = os.Mkdir(filepath.Join(dst, "dir"), 0o755)
	for _, name := range []string{"linked", "sym", "dir"} {
		st := stageSingle(t, dst, name, []byte("new"), ow)
		if _, err := st.Publish(); proto.CodeOf(err) != proto.CodeConflict {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "linked")); string(b) != "l" {
		t.Fatal("hard link replaced")
	}
	// A file where a destination folder is needed.
	if _, err := NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), "keep.txt/x", ops.FileKindFile, stageOpts()); proto.CodeOf(err) != proto.CodeConflict {
		t.Fatalf("file as folder: %v", err)
	}
	noParts(t, dst)
}

func TestPublishRequiresVerify(t *testing.T) {
	dst := t.TempDir()
	st, err := NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), "f", ops.FileKindFile, stageOpts())
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ReceiveFile(bytes.NewReader([]byte("abc")), st, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Publish(); proto.CodeOf(err) != proto.CodeIntegrityFailed {
		t.Fatalf("unverified publish: %v", err)
	}
	m := SingleFileManifest("f", 3, sum)
	if err := st.Verify(m, make([]byte, 32)); proto.CodeOf(err) != proto.CodeIntegrityFailed {
		t.Fatalf("wrong digest: %v", err)
	}
	wrong := SingleFileManifest("f", 3, sha256.Sum256([]byte("abd")))
	d, _ := wrong.Digest()
	if err := st.Verify(wrong, d[:]); proto.CodeOf(err) != proto.CodeIntegrityFailed {
		t.Fatalf("wrong content digest: %v", err)
	}
	_ = st.Abort()
	if got := entriesBelow(t, dst); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	// Short and long single-file bodies.
	st, _ = NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), "g", ops.FileKindFile, stageOpts())
	if _, err := ReceiveFile(bytes.NewReader([]byte("ab")), st, 3, time.Now()); proto.CodeOf(err) != proto.CodeIntegrityFailed {
		t.Fatalf("short: %v", err)
	}
	_ = st.Abort()
	st, _ = NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), "g", ops.FileKindFile, stageOpts())
	if _, err := ReceiveFile(bytes.NewReader([]byte("abcd")), st, 3, time.Now()); proto.CodeOf(err) != proto.CodeIntegrityFailed {
		t.Fatalf("long: %v", err)
	}
	_ = st.Abort()
	if got := entriesBelow(t, dst); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

func TestDiskFullLeavesNothing(t *testing.T) {
	src := buildTree(t)
	body, digest, _ := sendDir(t, src, "proj", proto.EncodingZstd)
	writes := 0
	stageWriteHook = func() error {
		writes++
		if writes > 2 {
			return unix.ENOSPC
		}
		return nil
	}
	defer func() { stageWriteHook = nil }()
	dst := t.TempDir()
	_, err := receiveDir(t, dst, "out", proto.EncodingZstd, body, digest[:], stageOpts())
	if proto.CodeOf(err) != proto.CodeInternal || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("got %v", err)
	}
	if got := entriesBelow(t, dst); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

func TestFreeSpaceReserve(t *testing.T) {
	dst := t.TempDir()
	opts := StageOptions{MinFreeBytes: 1 << 62}
	st, err := NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), "f", ops.FileKindFile, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Abort() }()
	if _, err := ReceiveFile(bytes.NewReader([]byte("abc")), st, 3, time.Now()); proto.CodeOf(err) != proto.CodeTransferTooLarge {
		t.Fatalf("got %v", err)
	}
}

func TestLimitsOnReceive(t *testing.T) {
	a := newArchive(t)
	a.file("a", make([]byte, 100), 0o644)
	a.file("b", make([]byte, 100), 0o644)
	a.manifest()
	body := a.bytes()
	for name, lim := range map[string]Limits{"bytes": {MaxBytes: 150}, "entries": {MaxEntries: 1}} {
		opts := stageOpts()
		opts.Limits = lim
		dst := t.TempDir()
		_, err := receiveDir(t, dst, "out", proto.EncodingIdentity, body, a.digest(), opts)
		if proto.CodeOf(err) != proto.CodeTransferTooLarge {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The Check hook refuses push-protected names.
	b := newArchive(t)
	b.dir(".git")
	b.dir(".git/hooks")
	b.file(".git/hooks/pre-commit", []byte("x"), 0o755)
	b.manifest()
	opts := stageOpts()
	opts.Check = func(rel string, _ bool) error {
		if ProtectedPath(pathElems(rel)) {
			return proto.NewError(proto.CodePathProtected, "protected")
		}
		return nil
	}
	dst := t.TempDir()
	if _, err := receiveDir(t, dst, "", proto.EncodingIdentity, b.bytes(), b.digest(), opts); proto.CodeOf(err) != proto.CodePathProtected {
		t.Fatalf("protected: %v", err)
	}
	if got := entriesBelow(t, dst); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

func TestWalkLimitsAndSingleSource(t *testing.T) {
	src := buildTree(t)
	root := newTestRoot(t, src)
	noop := func(Entry, *os.File) error { return nil }
	if _, err := Walk(context.Background(), root, "proj", WalkOptions{Limits: Limits{MaxEntries: 2}}, false, noop); proto.CodeOf(err) != proto.CodeTransferTooLarge {
		t.Fatalf("entries: %v", err)
	}
	if _, err := Walk(context.Background(), root, "proj", WalkOptions{Limits: Limits{MaxBytes: 1000}}, true, noop); proto.CodeOf(err) != proto.CodeTransferTooLarge {
		t.Fatalf("bytes: %v", err)
	}
	stats, err := Walk(context.Background(), root, "proj", WalkOptions{Filter: pullFilter}, false, noop)
	if err != nil || stats.Files != 3 || stats.Dirs != 3 {
		t.Fatalf("count: %+v %v", stats, err)
	}
	// Single sources.
	for p, code := range map[string]proto.Code{
		"proj/passwd": proto.CodeFileTypeRefused, "proj/h1": proto.CodeFileTypeRefused, "proj/fifo": proto.CodeFileTypeRefused,
		"proj/sub": proto.CodeFileTypeRefused, "proj/missing": proto.CodeNotFound, "proj/sublink/b.bin": proto.CodeFileTypeRefused,
		"../x": proto.CodePathOutsideRoot,
	} {
		if _, err := OpenSourceFile(root, p); proto.CodeOf(err) != code {
			t.Fatalf("%s: %v", p, err)
		}
	}
	sf, err := OpenSourceFile(root, "proj/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sf.Close() }()
	var out bytes.Buffer
	sum, err := CopyFile(&out, sf.File, sf.Size)
	if err != nil || out.String() != "alpha\n" || sum != sha256.Sum256([]byte("alpha\n")) {
		t.Fatalf("copy: %v", err)
	}
	if changed, err := sf.Changed(); changed || err != nil {
		t.Fatal("changed")
	}
	write(t, filepath.Join(src, "proj/a.txt"), []byte("alpha and more\n"))
	if changed, _ := sf.Changed(); !changed {
		t.Fatal("change not seen")
	}
}

func TestCleanup(t *testing.T) {
	dst := t.TempDir()
	id, other := ops.NewUUIDv7(), ops.NewUUIDv7()
	write(t, filepath.Join(dst, proto.StagingPrefix+id+"-1"), []byte("x"))
	write(t, filepath.Join(dst, proto.StagingPrefix+other+"-1"), []byte("x"))
	write(t, filepath.Join(dst, "user-file"), []byte("x"))
	_ = os.Mkdir(filepath.Join(dst, proto.StagingPrefix+"dir"), 0o755)
	_ = os.Symlink("user-file", filepath.Join(dst, proto.StagingPrefix+"link"))
	dir, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	n, err := CleanStaleParts(dir, id, proto.StaleStagingAge, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("own parts: %d %v", n, err)
	}
	n, _ = CleanStaleParts(dir, "", proto.StaleStagingAge, time.Now())
	if n != 0 {
		t.Fatalf("fresh part removed")
	}
	n, _ = CleanStaleParts(dir, "", proto.StaleStagingAge, time.Now().Add(25*time.Hour))
	if n != 1 {
		t.Fatalf("stale parts: %d", n)
	}
	got := entriesBelow(t, dst)
	if strings.Join(got, ",") != proto.StagingPrefix+"dir,"+proto.StagingPrefix+"link,user-file" {
		t.Fatalf("left %v", got)
	}

	// The staging log: a crash leaves parts; Sweep at start-up removes them.
	state := t.TempDir()
	log := NewStagingLog(state)
	st, err := NewStager(newTestRoot(t, dst), ops.NewUUIDv7(), "sub/f", ops.FileKindFile, stageOpts())
	if err != nil {
		t.Fatal(err)
	}
	rel, dev, ino := st.StagingDir()
	if rel != "" {
		t.Fatalf("staging dir %q", rel)
	}
	if err := log.Add(StagingRecord{Dir: filepath.Join(dst, rel), Dev: dev, Ino: ino, TransferID: st.transferID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReceiveFile(bytes.NewReader([]byte("abc")), st, 3, time.Now()); err != nil {
		t.Fatal(err)
	}
	// No Abort: the process "crashed".
	n, err = log.Sweep(proto.StaleStagingAge, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	noParts(t, dst)
	if recs, _ := log.load(); len(recs) != 0 {
		t.Fatalf("records left: %v", recs)
	}
	_ = st.Abort()
}
