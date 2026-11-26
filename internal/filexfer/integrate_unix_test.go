//go:build linux || darwin

package filexfer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// TestEngineWalkResolver runs a folder round trip through real roots of every
// resolver of this host, with the pull filter and the push check built from
// the FR-8 lists.
func TestEngineWalkResolver(t *testing.T) {
	for _, mode := range testModes(t) {
		t.Run(mode.String(), func(t *testing.T) {
			src := buildTree(t)
			sroot := newTestRootMode(t, src, mode)
			var body bytes.Buffer
			ww := NewWireWriter(&body)
			zw, err := NewEncoder(ww, proto.EncodingZstd)
			if err != nil {
				t.Fatal(err)
			}
			res, err := PackDir(context.Background(), zw, sroot, "proj", WalkOptions{Filter: PullFilter(sroot, nil)})
			if err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			skips := map[string]string{}
			for _, s := range res.Stats.Skips {
				skips[s.Path] = s.Reason
			}
			if skips[".ssh"] != proto.SkipSensitive || skips["passwd"] != proto.SkipSymlink || skips["fifo"] != proto.SkipSpecial {
				t.Fatalf("skips %v", skips)
			}
			digest, err := res.Manifest.Digest()
			if err != nil {
				t.Fatal(err)
			}
			dst := t.TempDir()
			droot := newTestRootMode(t, dst, mode)
			opts := stageOpts()
			opts.Check = PushCheck(droot, nil)
			st, err := NewStager(droot, ops.NewUUIDv7(), "copy", ops.FileKindDir, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Abort() }()
			dec, err := NewDecoder(NewWireReader(bytes.NewReader(body.Bytes()), 0), proto.EncodingZstd, opts.Limits)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dec.Close() }()
			m, err := Unpack(dec, st)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Verify(m, digest[:]); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Publish(); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(entriesBelow(t, filepath.Join(dst, "copy")), ",")
			if got != "a.txt,empty,sub,sub/b.bin,sub/deep,sub/deep/c" {
				t.Fatalf("published %s", got)
			}
			noParts(t, dst)
		})
	}
}

// TestEngineRefusalCodes checks that confinement refusals reach the engine's
// callers with their codes (proto.CodeOf and ProtoError).
func TestEngineRefusalCodes(t *testing.T) {
	src := buildTree(t)
	root := newTestRoot(t, src)
	if _, err := OpenSourceFile(root, "proj/passwd"); proto.CodeOf(err) != proto.CodeFileTypeRefused || ProtoError(err).Code != proto.CodeFileTypeRefused {
		t.Fatalf("symlink file: %v", err)
	}
	if _, err := OpenSourceFile(root, "proj/sublink/b.bin"); proto.CodeOf(err) != proto.CodeFileTypeRefused {
		t.Fatalf("symlink on the way: %v", err)
	}
	noop := func(Entry, *os.File) error { return nil }
	if _, err := Walk(context.Background(), root, "proj/sublink", WalkOptions{}, false, noop); proto.CodeOf(err) != proto.CodeFileTypeRefused {
		t.Fatalf("symlink folder: %v", err)
	}
	if _, err := OpenSourceFile(root, "proj/missing"); proto.CodeOf(err) != proto.CodeNotFound {
		t.Fatalf("missing: %v", err)
	}
	sf, err := OpenSourceFile(root, "proj/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = sf.Close()
	r := refuse(proto.CodePathSensitive, proto.SkipSensitive, "x", nil)
	if proto.CodeOf(r) != proto.CodePathSensitive {
		t.Fatalf("CodeOf(Refusal) = %s", proto.CodeOf(r))
	}
}

func TestPolicyHelpers(t *testing.T) {
	if proto.CodeOf(CheckPull(".ssh/id_ed25519")) != proto.CodePathSensitive || CheckPull("src/main.go") != nil || CheckPull("") != nil {
		t.Fatal("CheckPull")
	}
	base := tempDir(t)
	mustMkdir(t, filepath.Join(base, "proj"))
	mustMkdir(t, filepath.Join(base, "proj/.claude"))
	root := newTestRoot(t, filepath.Join(base, "proj"))
	f := PullFilter(root, func(rel string, _ Entry) string {
		if rel == "drop" {
			return proto.SkipName
		}
		return ""
	})
	if f("a/.aws", Entry{Type: proto.FileTypeDir}) != proto.SkipSensitive || f("x/tls.key", Entry{Type: proto.FileTypeFile}) != proto.SkipSensitive ||
		f("drop", Entry{Type: proto.FileTypeFile}) != proto.SkipName || f("ok.txt", Entry{Type: proto.FileTypeFile}) != "" {
		t.Fatal("PullFilter")
	}
	check := PushCheck(root, nil)
	if proto.CodeOf(check(".git/hooks/pre-commit", false)) != proto.CodePathProtected || proto.CodeOf(check(".env", false)) != proto.CodePathProtected || check("src/a.go", false) != nil {
		t.Fatal("PushCheck")
	}
	// A root inside a push-protected folder refuses every entry, unless the
	// owner set ProtectedOff.
	inner := newTestRoot(t, filepath.Join(base, "proj/.claude"))
	if !inner.PushProtected() || proto.CodeOf(PushCheck(inner, nil)("a.txt", false)) != proto.CodePathProtected {
		t.Fatal("PushCheck of a protected root")
	}
	floor := NewFloor(FloorConfig{ProtectedOff: true, System: []string{}, SystemDeep: []string{}})
	off, err := OpenDirRoot(filepath.Join(base, "proj/.claude"), RootOptions{Mode: engineMode(t), Floor: floor})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = off.Close() }()
	if err := PushCheck(off, nil)(".env", false); err != nil {
		t.Fatalf("ProtectedOff: %v", err)
	}
}
