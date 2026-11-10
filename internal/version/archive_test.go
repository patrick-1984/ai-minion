package version

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveFile struct {
	name string
	data []byte
	dir  bool
}

func makeTarGz(t testing.TB, files ...archiveFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}
		if f.dir {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t testing.TB, files ...archiveFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		name := f.name
		if f.dir {
			name += "/"
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	bin := []byte("\x7fELF pretend binary")
	sum := sha256.Sum256(bin)
	wantSum := hex.EncodeToString(sum[:])
	readme := archiveFile{name: "README.md", data: []byte("hi")}

	cases := []struct {
		name    string
		archive string
		goos    string
		data    []byte
		wantErr string
	}{
		{"tar.gz", "m.tar.gz", "linux", makeTarGz(t, readme, archiveFile{name: "minion", data: bin}), ""},
		{"tar.gz in a directory", "m.tar.gz", "darwin", makeTarGz(t, archiveFile{name: "minion_1.0.0", dir: true}, archiveFile{name: "minion_1.0.0/minion", data: bin}), ""},
		{"zip", "m.zip", "windows", makeZip(t, readme, archiveFile{name: "minion.exe", data: bin}), ""},
		{"tar.gz without the binary", "m.tar.gz", "linux", makeTarGz(t, readme), "does not contain"},
		{"directory named minion only", "m.tar.gz", "linux", makeTarGz(t, archiveFile{name: "minion", dir: true}), "does not contain"},
		{"empty binary", "m.tar.gz", "linux", makeTarGz(t, archiveFile{name: "minion", data: nil}), "empty"},
		{"zip with the wrong OS binary", "m.zip", "windows", makeZip(t, archiveFile{name: "minion", data: bin}), "does not contain"},
		{"corrupt tar.gz", "m.tar.gz", "linux", []byte("not gzip"), "cannot read"},
		{"corrupt zip", "m.zip", "windows", []byte("not zip"), "cannot read"},
		{"unknown type", "m.rar", "linux", []byte("x"), "unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ap := filepath.Join(dir, tc.archive)
			if err := os.WriteFile(ap, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, "out")
			af, oerr := os.Open(ap)
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer func() { _ = af.Close() }()
			got, err := extractBinary(af, tc.archive, tc.goos, dst)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if _, serr := os.Stat(dst); serr == nil {
					t.Error("partial output was left behind")
				}
				return
			}
			if err != nil || got != wantSum {
				t.Fatalf("extractBinary = %q, %v; want %q", got, err, wantSum)
			}
			data, _ := os.ReadFile(dst)
			if !bytes.Equal(data, bin) {
				t.Errorf("extracted %q", data)
			}
		})
	}
}

func TestBinaryName(t *testing.T) {
	if BinaryName("windows") != "minion.exe" || BinaryName("linux") != "minion" || BinaryName("darwin") != "minion" {
		t.Error("BinaryName")
	}
}
