package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/sqlmigrate"
)

var fixedTime = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func fixedClock() time.Time { return fixedTime }

// newHubDir builds a data directory with a database holding one setting, the
// data key, an identity key and generated TLS material.
func newHubDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(context.Background(), "public_url", "https://hub.example", false, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(context.Background(), "oidc_client_secret", "s3cret-value", true, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, IdentityKeyFile), "identity-key-bytes")
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "tls", "cert.pem"), "CERT")
	writeFile(t, filepath.Join(dir, "tls", "key.pem"), "TLSKEY")
	return dir
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type entry struct {
	hdr  tar.Header
	body []byte
}

// readArchive returns every entry of a backup archive in order.
func readArchive(t *testing.T, p string) []entry {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []entry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry{hdr: *h, body: b})
	}
}

// writeRaw writes a tar.gz of entries (name to body) for crafted-archive tests.
func writeRaw(t *testing.T, p string, entries []entry) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.hdr
		h.Size = int64(len(e.body))
		if h.Mode == 0 {
			h.Mode = 0o600
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func manifestBody(t *testing.T, m Manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func regular(name string) tar.Header { return tar.Header{Typeflag: tar.TypeReg, Name: name} }

func TestBackupArchiveContentsAndManifest(t *testing.T) {
	dir := newHubDir(t)
	out := filepath.Join(t.TempDir(), "out", "hub.tar.gz")

	m, err := Backup(context.Background(), dir, out, "v1.2.3", WithClock(fixedClock))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("archive mode = %v, want 0600", st.Mode().Perm())
		}
	}

	entries := readArchive(t, out)
	var names []string
	for _, e := range entries {
		names = append(names, e.hdr.Name)
		if e.hdr.Mode != 0o600 || e.hdr.Typeflag != tar.TypeReg {
			t.Errorf("%s: mode %o type %c", e.hdr.Name, e.hdr.Mode, e.hdr.Typeflag)
		}
		if e.hdr.Uid != 0 || e.hdr.Uname != "" {
			t.Errorf("%s: owner information leaked: uid %d uname %q", e.hdr.Name, e.hdr.Uid, e.hdr.Uname)
		}
	}
	want := []string{"manifest.json", "data.key", "identity.key", "tls/cert.pem", "tls/key.pem", "minion.db"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", names, want)
	}

	var got Manifest
	if err := json.Unmarshal(entries[0].body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Format != ManifestFormat || got.Version != "v1.2.3" || got.CreatedAt != "2026-11-09T10:00:00Z" {
		t.Fatalf("manifest = %+v", got)
	}
	embedded, err := sqlmigrate.Load(hubMigrations, migrationsDir)
	if err != nil || len(embedded) == 0 {
		t.Fatalf("load the embedded migrations: %v", err)
	}
	if want := embedded[len(embedded)-1].Version; got.SchemaVersion != want {
		t.Fatalf("schema version = %d, want %d (the newest embedded Hub migration)", got.SchemaVersion, want)
	}
	if got.SchemaVersion != m.SchemaVersion || len(got.Files) != 5 {
		t.Fatalf("returned manifest %+v differs from the archived one %+v", m, got)
	}
	for i, e := range entries[1:] {
		f := got.Files[i]
		if f.Name != e.hdr.Name || f.Size != int64(len(e.body)) || f.SHA256 != sumHex(e.body) {
			t.Errorf("manifest entry %+v does not match archive entry %s", f, e.hdr.Name)
		}
	}

	// The archived files are the data directory's files, the database a copy.
	body := map[string][]byte{}
	for _, e := range entries {
		body[e.hdr.Name] = e.body
	}
	if string(body["data.key"]) != readFile(t, filepath.Join(dir, "data.key")) ||
		string(body["identity.key"]) != "identity-key-bytes" ||
		string(body["tls/cert.pem"]) != "CERT" || string(body["tls/key.pem"]) != "TLSKEY" {
		t.Fatal("archived key or TLS files differ from the data directory")
	}
	if !bytes.HasPrefix(body["minion.db"], []byte("SQLite format 3\x00")) {
		t.Fatal("minion.db is not a SQLite database")
	}
	if bytes.Contains(body["minion.db"], []byte("s3cret-value")) {
		t.Fatal("a sealed secret appears in clear text in the database copy")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".minion-backup-*"))
	if len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
}

func TestBackupOptionalFilesAreSkipped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	m, err := Backup(context.Background(), dir, out, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 || m.Files[0].Name != "data.key" || m.Files[1].Name != "minion.db" {
		t.Fatalf("files = %+v", m.Files)
	}
}

func TestBackupWhileHubHasTheDatabaseOpen(t *testing.T) {
	dir := newHubDir(t)
	s, err := Open(dir) // a running Hub: WAL mode, connection open
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutSetting(context.Background(), "later", "v", false, "test"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(context.Background(), dir, out, "dev"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(context.Background(), out, target, false); err != nil {
		t.Fatal(err)
	}
	r, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, ok, err := r.GetSetting(context.Background(), "later"); err != nil || !ok {
		t.Fatalf("a row written before the backup is missing: ok=%v err=%v", ok, err)
	}
}

func TestBackupRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("not a hub directory", func(t *testing.T) {
		empty := t.TempDir()
		_, err := Backup(ctx, empty, filepath.Join(t.TempDir(), "x.tar.gz"), "dev")
		if !errors.Is(err, ErrBackup) {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(empty, DBFile)); err == nil {
			t.Fatal("backup created a database in a directory that had none")
		}
	})
	t.Run("existing output", func(t *testing.T) {
		dir := newHubDir(t)
		out := filepath.Join(t.TempDir(), "x.tar.gz")
		writeFile(t, out, "keep me")
		_, err := Backup(ctx, dir, out, "dev")
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("err = %v", err)
		}
		if readFile(t, out) != "keep me" {
			t.Fatal("existing output file was modified")
		}
	})
	t.Run("not a sqlite file", func(t *testing.T) {
		dir := newHubDir(t)
		writeFile(t, filepath.Join(dir, DBFile), strings.Repeat("not a database ", 100))
		out := filepath.Join(t.TempDir(), "x.tar.gz")
		if _, err := Backup(ctx, dir, out, "dev"); !errors.Is(err, ErrBackup) {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("a partial archive was left behind")
		}
	})
	t.Run("empty arguments", func(t *testing.T) {
		if _, err := Backup(ctx, "", "x", "dev"); !errors.Is(err, ErrBackup) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, dir, out, "dev"); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "new", "hub") // does not exist
	res, err := Restore(ctx, out, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != target || strings.Join(res.Files, ",") != "data.key,identity.key,tls/cert.pem,tls/key.pem,minion.db" {
		t.Fatalf("result = %+v", res)
	}
	for _, name := range []string{"data.key", "identity.key", "tls/cert.pem", "tls/key.pem"} {
		if readFile(t, filepath.Join(target, name)) != readFile(t, filepath.Join(dir, name)) {
			t.Errorf("%s is not byte-identical after restore", name)
		}
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{"data.key", "identity.key", "tls/cert.pem", "tls/key.pem", "minion.db"} {
			st, err := os.Stat(filepath.Join(target, name))
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", name, st.Mode().Perm())
			}
			if st.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s is readable by others", name)
			}
		}
		for _, d := range []string{target, filepath.Join(target, "tls")} {
			st, err := os.Stat(d)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o700 {
				t.Errorf("%s mode = %v, want 0700", d, st.Mode().Perm())
			}
		}
	}
	if left, _ := filepath.Glob(filepath.Join(target, ".restore-*")); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}

	// The restored directory opens as a Hub and reads the old data with the old key.
	s, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec, ok, err := s.GetSetting(ctx, "oidc_client_secret")
	if err != nil || !ok || rec.Value != "s3cret-value" {
		t.Fatalf("sealed setting after restore: %+v ok=%v err=%v", rec, ok, err)
	}
	if rec, ok, err := s.GetSetting(ctx, "public_url"); err != nil || !ok || rec.Value != "https://hub.example" {
		t.Fatalf("setting after restore: %+v ok=%v err=%v", rec, ok, err)
	}
}

func TestRestoreRefusesNonEmptyDirWithoutForce(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, dir, out, "dev"); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	writeFile(t, filepath.Join(target, "something"), "x")
	_, err := Restore(ctx, out, target, false)
	if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 1 {
		t.Fatalf("a refused restore changed the directory: %v", entries)
	}

	// An existing empty directory is fine, and so is force over a live-looking one.
	if _, err := Restore(ctx, out, t.TempDir(), false); err != nil {
		t.Fatalf("empty existing dir: %v", err)
	}
	if _, err := Restore(ctx, out, target, true); err != nil {
		t.Fatalf("force: %v", err)
	}
	if readFile(t, filepath.Join(target, "something")) != "x" {
		t.Fatal("force removed a file that is not part of the backup")
	}
}

func TestRestoreForceReplacesDatabaseAndStaleWAL(t *testing.T) {
	ctx := context.Background()
	src := newHubDir(t)
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, src, out, "dev"); err != nil {
		t.Fatal(err)
	}

	// A different Hub in the target, with leftovers of a crashed database.
	target := newHubDir(t)
	writeFile(t, filepath.Join(target, "minion.db-wal"), "stale wal")
	writeFile(t, filepath.Join(target, "minion.db-shm"), "stale shm")
	if err := os.MkdirAll(filepath.Join(target, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "backups", "minion-old.db"), "old")

	if _, err := Restore(ctx, out, target, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"minion.db-wal", "minion.db-shm"} {
		if _, err := os.Stat(filepath.Join(target, p)); err == nil {
			t.Errorf("%s survived the restore", p)
		}
	}
	if readFile(t, filepath.Join(target, "backups", "minion-old.db")) != "old" {
		t.Error("backups/ was touched")
	}
	if readFile(t, filepath.Join(target, "data.key")) != readFile(t, filepath.Join(src, "data.key")) {
		t.Error("data.key was not replaced")
	}
	s, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.GetSetting(ctx, "public_url"); err != nil || !ok {
		t.Fatalf("restored database unreadable: ok=%v err=%v", ok, err)
	}
}

func TestRestoreForceRemovesIdentityNotInArchive(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "hub")
	s, err := Open(src) // a backup without identity key and TLS material
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, src, out, "dev"); err != nil {
		t.Fatal(err)
	}
	target := newHubDir(t)
	if _, err := Restore(ctx, out, target, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, IdentityKeyFile)); err == nil {
		t.Fatal("the old identity key stayed next to a restored database that does not use it")
	}
}

func TestRestoreRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "hub")
	s, err := Open(dir, testMigrations(v2))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	m, err := Backup(ctx, dir, out, "v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 2 {
		t.Fatalf("schema = %d", m.SchemaVersion)
	}

	target := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(ctx, out, target, false, testMigrations(v1))
	if !errors.Is(err, sqlmigrate.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want schema_too_new", err)
	}
	if !strings.Contains(err.Error(), "v9.9.9") {
		t.Errorf("error does not name the writing version: %v", err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a refused restore created the target directory")
	}
	// A binary that knows the schema accepts it.
	if _, err := Restore(ctx, out, target, false, testMigrations(v2)); err != nil {
		t.Fatal(err)
	}
}

// validParts returns the entries of a minimal valid archive.
func validParts(t *testing.T) (m Manifest, bodies map[string][]byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	out := filepath.Join(t.TempDir(), "good.tar.gz")
	if _, err := Backup(context.Background(), dir, out, "dev"); err != nil {
		t.Fatal(err)
	}
	es := readArchive(t, out)
	if err := json.Unmarshal(es[0].body, &m); err != nil {
		t.Fatal(err)
	}
	bodies = map[string][]byte{}
	for _, e := range es[1:] {
		bodies[e.hdr.Name] = e.body
	}
	return m, bodies
}

func TestRestoreRefusesBadArchives(t *testing.T) {
	ctx := context.Background()
	m, bodies := validParts(t)
	dir := t.TempDir()

	build := func(mut func(m *Manifest, entries *[]entry)) string {
		mm := m
		mm.Files = append([]ManifestEntry(nil), m.Files...)
		es := []entry{{hdr: regular("manifest.json")}}
		for _, f := range mm.Files {
			es = append(es, entry{hdr: regular(f.Name), body: bodies[f.Name]})
		}
		mut(&mm, &es)
		es[0].body = manifestBody(t, mm)
		p := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+time.Now().Format("150405.000000000")+".tar.gz")
		writeRaw(t, p, es)
		return p
	}

	cases := map[string]struct {
		mut  func(m *Manifest, es *[]entry)
		want string
	}{
		"valid control": {func(m *Manifest, es *[]entry) {}, ""},
		"checksum mismatch": {func(m *Manifest, es *[]entry) {
			(*es)[1].body = append([]byte(nil), (*es)[1].body...)
			(*es)[1].body[0] ^= 0xff
		}, "checksum"},
		"missing file": {func(m *Manifest, es *[]entry) { *es = (*es)[:len(*es)-1] }, "incomplete"},
		"extra file": {func(m *Manifest, es *[]entry) {
			*es = append(*es, entry{hdr: regular("evil"), body: []byte("x")})
		}, "unexpected entry"},
		"path traversal": {func(m *Manifest, es *[]entry) {
			*es = append(*es, entry{hdr: regular("../escape"), body: []byte("x")})
		}, "unexpected entry"},
		"symlink": {func(m *Manifest, es *[]entry) {
			(*es)[2].hdr.Typeflag = tar.TypeSymlink
			(*es)[2].hdr.Linkname = "/etc/passwd"
			(*es)[2].body = nil
			m.Files[1].Size = 0
		}, "not a regular file"},
		"manifest lists traversal": {func(m *Manifest, es *[]entry) {
			m.Files = append(m.Files, ManifestEntry{Name: "../x", SHA256: strings.Repeat("0", 64)})
		}, "unexpected file"},
		"duplicate manifest entry": {func(m *Manifest, es *[]entry) { m.Files = append(m.Files, m.Files[0]) }, "twice"},
		"no database in manifest": {func(m *Manifest, es *[]entry) {
			m.Files = m.Files[:1]
			*es = (*es)[:2]
		}, "does not contain"},
		"wrong format":  {func(m *Manifest, es *[]entry) { m.Format = 99 }, "format"},
		"size mismatch": {func(m *Manifest, es *[]entry) { m.Files[1].Size++ }, "size"},
		"manifest schema differs from database": {func(m *Manifest, es *[]entry) {
			m.SchemaVersion = 1
		}, "manifest says"},
		"manifest not first": {func(m *Manifest, es *[]entry) { (*es)[0], (*es)[1] = (*es)[1], (*es)[0] }, "not a Hub backup"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := build(tc.mut)
			target := filepath.Join(t.TempDir(), "t")
			if name == "manifest not first" {
				// the swap above also swaps the manifest body back in place
				es := []entry{{hdr: regular("data.key"), body: bodies["data.key"]}, {hdr: regular("manifest.json"), body: manifestBody(t, m)}}
				writeRaw(t, p, es)
			}
			_, err := Restore(ctx, p, target, false)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if entries, _ := os.ReadDir(target); len(entries) != 0 {
				t.Fatalf("a refused restore left files behind: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(target), "escape")); err == nil {
				t.Fatal("path traversal wrote outside the target")
			}
		})
	}
}

func TestRestoreRefusesNonArchives(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	writeFile(t, plain, "just text")
	if _, err := Restore(ctx, plain, filepath.Join(dir, "t"), false); !errors.Is(err, ErrBackup) {
		t.Fatalf("plain file: %v", err)
	}
	if _, err := Restore(ctx, filepath.Join(dir, "missing"), filepath.Join(dir, "t"), false); !errors.Is(err, ErrBackup) {
		t.Fatalf("missing file: %v", err)
	}
	empty := filepath.Join(dir, "empty.tar.gz")
	writeRaw(t, empty, nil)
	if _, err := Restore(ctx, empty, filepath.Join(dir, "t"), false); !errors.Is(err, ErrBackup) {
		t.Fatalf("empty archive: %v", err)
	}
	if _, err := Restore(ctx, plain, "", false); !errors.Is(err, ErrBackup) {
		t.Fatalf("empty target: %v", err)
	}
	file := filepath.Join(dir, "afile")
	writeFile(t, file, "x")
	m, bodies := validParts(t)
	good := filepath.Join(dir, "good.tar.gz")
	es := []entry{{hdr: regular("manifest.json"), body: manifestBody(t, m)}}
	for _, f := range m.Files {
		es = append(es, entry{hdr: regular(f.Name), body: bodies[f.Name]})
	}
	writeRaw(t, good, es)
	if _, err := Restore(ctx, good, file, true); !errors.Is(err, ErrBackup) {
		t.Fatalf("target is a file: %v", err)
	}
}

func TestRestoreRefusesCorruptDatabase(t *testing.T) {
	ctx := context.Background()
	m, bodies := validParts(t)
	db := append([]byte(nil), bodies[DBFile]...)
	for i := 4096; i < len(db) && i < 4096+512; i++ {
		db[i] = 0xAB // damage the second page: checksums match the damaged bytes
	}
	for i := range m.Files {
		if m.Files[i].Name == DBFile {
			m.Files[i].Size = int64(len(db))
			m.Files[i].SHA256 = sumHex(db)
		}
	}
	es := []entry{{hdr: regular("manifest.json"), body: manifestBody(t, m)}}
	for _, f := range m.Files {
		b := bodies[f.Name]
		if f.Name == DBFile {
			b = db
		}
		es = append(es, entry{hdr: regular(f.Name), body: b})
	}
	p := filepath.Join(t.TempDir(), "bad.tar.gz")
	writeRaw(t, p, es)
	target := filepath.Join(t.TempDir(), "t")
	_, err := Restore(ctx, p, target, false)
	if !errors.Is(err, ErrBackup) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("files left behind: %v", entries)
	}
}

func TestBackupRestoreKeepsEveryByteOfTheIdentityFile(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	raw := make([]byte, 64)
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(dir, IdentityKeyFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, dir, out, "dev"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "n")
	if _, err := Restore(ctx, out, target, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, IdentityKeyFile))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("identity file differs after backup and restore: %v", err)
	}
}

func TestBackupRefusesAnUnusableDataKey(t *testing.T) {
	ctx := context.Background()
	t.Run("short key", func(t *testing.T) {
		dir := newHubDir(t)
		writeFile(t, filepath.Join(dir, DataKeyFile), "ninebytes")
		out := filepath.Join(t.TempDir(), "x.tar.gz")
		_, err := Backup(ctx, dir, out, "dev")
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "exactly 32 bytes") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("an archive was published")
		}
	})
	t.Run("directory instead of key", func(t *testing.T) {
		dir := newHubDir(t)
		if err := os.Remove(filepath.Join(dir, DataKeyFile)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, DataKeyFile), 0o700); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "x.tar.gz")
		_, err := Backup(ctx, dir, out, "dev")
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("an archive was published")
		}
	})
}

func TestRestoreRefusesAnUnusableDataKeyAndKeepsTheOldHub(t *testing.T) {
	ctx := context.Background()
	m, bodies := validParts(t)
	short := []byte("ninebytes")
	mm := m
	mm.Files = append([]ManifestEntry(nil), m.Files...)
	es := []entry{{hdr: regular("manifest.json")}}
	for i, f := range mm.Files {
		body := bodies[f.Name]
		if f.Name == DataKeyFile {
			body = short
			mm.Files[i].Size = int64(len(short))
			mm.Files[i].SHA256 = sumHex(short)
		}
		es = append(es, entry{hdr: regular(f.Name), body: body})
	}
	es[0].body = manifestBody(t, mm)
	archive := filepath.Join(t.TempDir(), "short.tar.gz")
	writeRaw(t, archive, es)

	target := newHubDir(t)
	oldKey := readFile(t, filepath.Join(target, DataKeyFile))
	_, err := Restore(ctx, archive, target, true)
	if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, filepath.Join(target, DataKeyFile)) != oldKey {
		t.Fatal("the old data key was replaced")
	}
}

// openMarked opens the Hub in dir and returns the value of the setting
// "marker", which identifies one Hub.
func openMarked(t *testing.T, dir string) string {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("the Hub in %s cannot be opened: %v", dir, err)
	}
	defer s.Close()
	v, _, err := s.GetSetting(context.Background(), "marker")
	if err != nil {
		t.Fatal(err)
	}
	return v.Value
}

// markedHubDir is a [newHubDir] with the setting marker=name.
func markedHubDir(t *testing.T, name string) string {
	t.Helper()
	dir := newHubDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(context.Background(), "marker", name, false, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRestoreFailureRollsBackToTheOldHub(t *testing.T) {
	ctx := context.Background()
	src := markedHubDir(t, "new")
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, src, archive, "dev"); err != nil {
		t.Fatal(err)
	}

	files := []string{DataKeyFile, IdentityKeyFile, "tls/cert.pem", "tls/key.pem"}
	// Fail at every installation step in turn (renames from the staging
	// directory), after the old files were already moved aside.
	for _, failAt := range []string{"data.key", "identity.key", "tls/cert.pem", "tls/key.pem", "minion.db"} {
		t.Run(failAt, func(t *testing.T) {
			target := markedHubDir(t, "old")
			before := map[string]string{}
			for _, f := range files {
				before[f] = readFile(t, filepath.Join(target, filepath.FromSlash(f)))
			}
			orig := installRename
			t.Cleanup(func() { installRename = orig })
			installRename = func(root *os.Root, from, to string) error {
				if to == failAt && strings.Contains(from, ".restore-") && !strings.Contains(from, "/old/") {
					return errors.New("injected failure")
				}
				return orig(root, from, to)
			}
			_, err := Restore(ctx, archive, target, true)
			if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("err = %v", err)
			}
			for _, f := range files {
				if got := readFile(t, filepath.Join(target, filepath.FromSlash(f))); got != before[f] {
					t.Errorf("%s changed by a failed restore", f)
				}
			}
			if got := openMarked(t, target); got != "old" {
				t.Fatalf("the Hub in the target is %q, want the old one", got)
			}
		})
	}
}

// When rolling back a failed install also fails, the previous files that
// could not be put back exist only in the staging directory. It must survive,
// the error must name it, and the files in it must be intact.
func TestRestoreKeepsTheOldFilesWhenRollbackFails(t *testing.T) {
	ctx := context.Background()
	src := markedHubDir(t, "new")
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, src, archive, "dev"); err != nil {
		t.Fatal(err)
	}
	target := markedHubDir(t, "old")
	oldKey := readFile(t, filepath.Join(target, DataKeyFile))
	oldIdentity := readFile(t, filepath.Join(target, IdentityKeyFile))
	oldDB := readFile(t, filepath.Join(target, DBFile))

	orig := installRename
	t.Cleanup(func() { installRename = orig })
	installRename = func(root *os.Root, from, to string) error {
		switch {
		case to == "tls/key.pem" && strings.Contains(from, ".restore-") && !strings.Contains(from, "/old/"):
			return errors.New("injected install failure")
		case (to == DataKeyFile || to == DBFile) && strings.Contains(from, "/old/"):
			return errors.New("injected rollback failure")
		}
		return orig(root, from, to)
	}
	_, err := Restore(ctx, archive, target, true)
	if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "injected install failure") || !strings.Contains(err.Error(), "injected rollback failure") {
		t.Fatalf("err = %v", err)
	}
	stages, gerr := filepath.Glob(filepath.Join(target, ".restore-*"))
	if gerr != nil || len(stages) != 1 {
		t.Fatalf("staging directories = %v, want exactly one kept", stages)
	}
	if !strings.Contains(err.Error(), filepath.Join(stages[0], "old")) {
		t.Errorf("the error does not name the recovery directory %s: %v", filepath.Join(stages[0], "old"), err)
	}
	// The files whose rollback failed are intact in the recovery directory;
	// the others went back to the data directory.
	if got := readFile(t, filepath.Join(stages[0], "old", DataKeyFile)); got != oldKey {
		t.Errorf("the previous data.key in the recovery directory changed")
	}
	if got := readFile(t, filepath.Join(stages[0], "old", DBFile)); got != oldDB {
		t.Errorf("the previous database in the recovery directory changed")
	}
	if got := readFile(t, filepath.Join(target, IdentityKeyFile)); got != oldIdentity {
		t.Errorf("identity.key was not put back")
	}
}

// A failure with a complete rollback leaves no staging directory behind.
func TestRestoreRemovesTheStagingDirectoryAfterACompleteRollback(t *testing.T) {
	ctx := context.Background()
	src := markedHubDir(t, "new")
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, src, archive, "dev"); err != nil {
		t.Fatal(err)
	}
	target := markedHubDir(t, "old")
	orig := installRename
	t.Cleanup(func() { installRename = orig })
	installRename = func(root *os.Root, from, to string) error {
		if to == "tls/key.pem" && strings.Contains(from, ".restore-") && !strings.Contains(from, "/old/") {
			return errors.New("injected install failure")
		}
		return orig(root, from, to)
	}
	if _, err := Restore(ctx, archive, target, true); err == nil {
		t.Fatal("want an error")
	}
	if stages, _ := filepath.Glob(filepath.Join(target, ".restore-*")); len(stages) != 0 {
		t.Fatalf("staging directories left behind: %v", stages)
	}
}

func TestRestoreRefusesToReplaceADirectoryAndKeepsTheOldHub(t *testing.T) {
	ctx := context.Background()
	src := markedHubDir(t, "new")
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, src, archive, "dev"); err != nil {
		t.Fatal(err)
	}
	target := markedHubDir(t, "old")
	oldKey := readFile(t, filepath.Join(target, DataKeyFile))
	cert := filepath.Join(target, "tls", "cert.pem")
	if err := os.Remove(cert); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cert, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(ctx, archive, target, true)
	if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, filepath.Join(target, DataKeyFile)) != oldKey {
		t.Fatal("data.key was removed or replaced")
	}
	if got := openMarked(t, target); got != "old" {
		t.Fatalf("Hub = %q", got)
	}
}

func TestRestoreDoesNotWriteThroughSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	ctx := context.Background()
	src := markedHubDir(t, "new")
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := Backup(ctx, src, archive, "dev"); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "bare") // an archive without TLS material
	s, err := Open(bare)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	bareArchive := filepath.Join(t.TempDir(), "bare.tar.gz")
	if _, err := Backup(ctx, bare, bareArchive, "dev"); err != nil {
		t.Fatal(err)
	}

	t.Run("tls directory is a symlink", func(t *testing.T) {
		for _, a := range []string{archive, bareArchive} {
			outside := t.TempDir()
			writeFile(t, filepath.Join(outside, "cert.pem"), "OUTSIDE-CERT")
			writeFile(t, filepath.Join(outside, "key.pem"), "OUTSIDE-KEY")
			target := markedHubDir(t, "old")
			if err := os.RemoveAll(filepath.Join(target, "tls")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(target, "tls")); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(ctx, a, target, true); !errors.Is(err, ErrBackup) {
				t.Fatalf("err = %v", err)
			}
			if readFile(t, filepath.Join(outside, "cert.pem")) != "OUTSIDE-CERT" || readFile(t, filepath.Join(outside, "key.pem")) != "OUTSIDE-KEY" {
				t.Fatal("files outside the data directory were changed")
			}
			if got := openMarked(t, target); got != "old" {
				t.Fatalf("Hub = %q", got)
			}
		}
	})
	t.Run("tls file is a symlink", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside.pem")
		writeFile(t, outside, "OUTSIDE-KEY")
		target := markedHubDir(t, "old")
		keyPath := filepath.Join(target, "tls", "key.pem")
		if err := os.Remove(keyPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, keyPath); err != nil {
			t.Fatal(err)
		}
		if _, err := Restore(ctx, archive, target, true); err != nil {
			t.Fatal(err)
		}
		if readFile(t, outside) != "OUTSIDE-KEY" {
			t.Fatal("the file behind the symlink was overwritten")
		}
		if readFile(t, keyPath) != "TLSKEY" {
			t.Fatal("the symlink was not replaced by the restored key")
		}
		if got := openMarked(t, target); got != "new" {
			t.Fatalf("Hub = %q", got)
		}
	})
}
