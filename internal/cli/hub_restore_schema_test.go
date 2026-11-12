package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// RC1 release gate, finding 6: restore names the schema of the archive and the
// schema of the installed minion separately, instead of printing the archive's
// schema as if it were the installed one.
func TestRunHubRestorePrintsArchiveAndInstalledSchema(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "hub")
	s, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	archive := filepath.Join(t.TempDir(), "hub.tar.gz")
	if err := runHubBackup(ctx, &bytes.Buffer{}, hubBackupDeps{}, src, archive, plainBackup); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	hostOnly := hubBackupDeps{containerDir: func(string) containerKind { return notContainer }}
	var out bytes.Buffer
	if err := runHubRestore(ctx, &out, hostOnly, dst, archive, false, "", 0); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`archive schema (\d+), written by minion [^;]+; installed schema (\d+)`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("output does not label both schemas: %q", out.String())
	}
	archived, _ := strconv.Atoi(m[1])
	installed, _ := strconv.Atoi(m[2])
	// The archive was written by this very build: both are the newest schema.
	if archived <= 0 || installed != archived {
		t.Errorf("archive schema %d, installed schema %d", archived, installed)
	}
}

// legacySchema20Archive writes a format-1 (0.6.x) archive whose database is
// migrated only up to schema 20, from the migration files of the Hub store.
func legacySchema20Archive(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	migDir := filepath.Join("..", "hub", "store", "migrations")
	ents, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatal(err)
	}
	mig := fstest.MapFS{}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0021" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		mig["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	dir := filepath.Join(t.TempDir(), "old")
	s, err := store.Open(dir, store.WithMigrations(mig, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snap.db")
	if _, err := s.DB().ExecContext(ctx, `VACUUM INTO ?`, snap); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	bodies := map[string][]byte{}
	for name, path := range map[string]string{store.DBFile: snap, store.DataKeyFile: filepath.Join(dir, store.DataKeyFile)} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		bodies[name] = b
	}
	m := store.Manifest{Format: 1, Version: "0.6.3", SchemaVersion: 20, CreatedAt: "2026-01-01T00:00:00Z"}
	names := []string{store.DataKeyFile, store.DBFile}
	for _, name := range names {
		sum := sha256.Sum256(bodies[name])
		m.Files = append(m.Files, store.ManifestEntry{Name: name, Size: int64(len(bodies[name])), SHA256: hex.EncodeToString(sum[:])})
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	write(store.ManifestFile, mb)
	for _, name := range names {
		write(name, bodies[name])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "legacy.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive
}

// RC2 release gate (H6 test gap): an archive of an older schema shows its own
// schema (20) and the installed one (23) separately.
func TestRunHubRestoreOfAnOlderSchemaArchiveShowsBothSchemas(t *testing.T) {
	archive := legacySchema20Archive(t)
	dst := filepath.Join(t.TempDir(), "restored")
	hostOnly := hubBackupDeps{containerDir: func(string) containerKind { return notContainer }}
	var out bytes.Buffer
	if err := runHubRestore(context.Background(), &out, hostOnly, dst, archive, false, "", 0); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`archive schema (\d+), written by minion [^;]+; installed schema (\d+)`).FindStringSubmatch(out.String())
	if m == nil || m[1] != "20" || m[2] != "23" {
		t.Fatalf("want archive schema 20 and installed schema 23 in %q", out.String())
	}
}
