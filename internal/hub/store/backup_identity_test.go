package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// idHub is a data directory in one bootstrap state.
type idHub struct {
	dir    string
	s      *Store
	signer *identity.RootSigner
	root   []byte // sealed root bytes (any bytes: backup never opens them)
	op     *identity.Key
}

func sha(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

func mustT(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newIDHub builds a Hub data directory in the state: none, plain,
// sealed_no_cert or established. The store stays open (closed at cleanup).
func newIDHub(t *testing.T, state string) *idHub {
	t.Helper()
	h := &idHub{dir: filepath.Join(t.TempDir(), "hub")}
	s, err := Open(h.dir)
	mustT(t, err)
	t.Cleanup(func() { _ = s.Close() })
	h.s = s
	ctx := context.Background()
	mustT(t, s.PutSetting(ctx, "oidc_client_secret", "s3cret-value", true, "test"))
	mustT(t, os.MkdirAll(filepath.Join(h.dir, "tls"), 0o700))
	writeFile(t, filepath.Join(h.dir, "tls", "cert.pem"), "CERT")
	writeFile(t, filepath.Join(h.dir, "tls", "key.pem"), "TLSKEY")
	switch state {
	case IdentityStateNone:
	case IdentityStatePlain:
		key, err := identity.GenerateKey()
		mustT(t, err)
		mustT(t, identity.SaveKey(filepath.Join(h.dir, IdentityKeyFile), key))
		mustT(t, s.PutHubIdentity(ctx, HubIdentityRow{Pub: key.Public(), Fingerprint: key.Fingerprint().String(), CreatedAt: 1}))
	default:
		seed, err := identity.NewRootSeed()
		mustT(t, err)
		h.signer, err = identity.RootSignerFromSeed(seed)
		mustT(t, err)
		fpr, err := identity.FingerprintOf(h.signer.Public())
		mustT(t, err)
		mustT(t, s.PutHubIdentity(ctx, HubIdentityRow{Pub: h.signer.Public(), Fingerprint: fpr.String(), CreatedAt: 1}))
		h.root = bytes.Repeat([]byte{7}, 120)
		mustT(t, os.WriteFile(filepath.Join(h.dir, SealedRootFile), h.root, 0o600))
		h.sql(`UPDATE hub_identity SET root_sealed = 1, root_file_sha256 = ?`, sha(h.root))
		if state == IdentityStateEstablished {
			h.issue(t, "")
		}
	}
	return h
}

func (h *idHub) sql(q string, args ...any) {
	if _, err := h.s.db.Exec(q, args...); err != nil {
		panic(err)
	}
}

// issue records a certificate for a new operational key and writes the key to
// the file name (op.key when empty).
func (h *idHub) issue(t *testing.T, file string) *identity.Key {
	t.Helper()
	if file == "" {
		file = OpKeyFile
	}
	op, err := identity.GenerateKey()
	mustT(t, err)
	cert, err := h.signer.IssueHubCert(op.Public(), 1, 1000, 2000)
	mustT(t, err)
	mustT(t, identity.SaveKey(filepath.Join(h.dir, file), op))
	h.sql(`UPDATE hub_identity SET current_cert = ?, current_serial = 1, last_issued = 1`, cert.Bytes())
	h.op = op
	return op
}

// setRoot replaces the recorded sealed root and the file name that holds it.
func (h *idHub) setRoot(t *testing.T, file string, body []byte) {
	t.Helper()
	mustT(t, os.WriteFile(filepath.Join(h.dir, file), body, 0o600))
	h.sql(`UPDATE hub_identity SET root_file_sha256 = ?`, sha(body))
}

var allStates = []string{IdentityStateNone, IdentityStatePlain, IdentityStateSealedNoCert, IdentityStateEstablished}

func archiveNames(t *testing.T, p string) []string {
	t.Helper()
	var names []string
	for _, e := range readArchive(t, p)[1:] {
		names = append(names, e.hdr.Name)
	}
	sort.Strings(names)
	return names
}

func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	mustT(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return err
	}))
	sort.Strings(out)
	return out
}

func wantIdentityFiles(state string) string {
	switch state {
	case IdentityStatePlain:
		return "identity.key"
	case IdentityStateSealedNoCert:
		return "root.key.age"
	case IdentityStateEstablished:
		return "op.key,root.key.age"
	}
	return ""
}

func identityNames(names []string) string {
	var out []string
	for _, n := range names {
		if isIdentityArchiveName(n) {
			out = append(out, n)
		}
	}
	return strings.Join(out, ",")
}

func TestBackupRestoreRoundTripPerState(t *testing.T) {
	ctx := context.Background()
	for _, state := range allStates {
		t.Run(state, func(t *testing.T) {
			h := newIDHub(t, state)
			out := filepath.Join(t.TempDir(), "b.tar.gz")
			m, err := Backup(ctx, h.dir, out, "dev", WithClock(fixedClock))
			mustT(t, err)
			if m.Format != 2 || m.IdentityState != state {
				t.Fatalf("manifest = %+v", m)
			}
			if got := identityNames(archiveNames(t, out)); got != wantIdentityFiles(state) {
				t.Fatalf("identity entries = %q, want %q", got, wantIdentityFiles(state))
			}
			if (state == IdentityStateNone) != (m.RootFingerprint == "") {
				t.Fatalf("root fingerprint = %q", m.RootFingerprint)
			}
			target := filepath.Join(t.TempDir(), "restored")
			res, err := Restore(ctx, out, target, false)
			mustT(t, err)
			if res.IdentityState != state || res.RootRolledBack {
				t.Fatalf("result = %+v", res)
			}
			for _, name := range []string{"data.key", "identity.key", "root.key.age", "op.key", "tls/key.pem"} {
				src, serr := os.ReadFile(filepath.Join(h.dir, name))
				got, gerr := os.ReadFile(filepath.Join(target, name))
				if (serr == nil) != (gerr == nil) || !bytes.Equal(src, got) {
					t.Errorf("%s differs after the round trip (%v, %v)", name, serr, gerr)
				}
			}
			// No -wal/-shm, no staging, no other file.
			for _, f := range dirFiles(t, target) {
				if strings.HasSuffix(f, "-wal") || strings.HasSuffix(f, "-shm") || strings.Contains(f, ".restore-") {
					t.Errorf("unexpected file %s", f)
				}
			}
			r, err := Open(target)
			mustT(t, err)
			defer r.Close()
			rec, ok, err := r.GetSetting(ctx, "oidc_client_secret")
			if err != nil || !ok || rec.Value != "s3cret-value" {
				t.Fatalf("the OIDC secret after restore: %+v %v %v", rec, ok, err)
			}
			// A restore issues no certificate: the row is the backed-up one.
			want, _, _ := h.s.HubIdentityV2(ctx)
			got, ok, err := r.HubIdentityV2(ctx)
			mustT(t, err)
			if ok != (state != IdentityStateNone) || !bytes.Equal(got.CurrentCert, want.CurrentCert) || got.LastIssued != want.LastIssued {
				t.Fatalf("identity row after restore: %+v", got)
			}
			// The whole row travels unchanged, whatever columns it has.
			if a, b := rowDump(t, h.s), rowDump(t, r); a != b {
				t.Fatalf("hub_identity changed in the round trip:\n%s\n%s", a, b)
			}
		})
	}
}

func TestSnapshotRetriesWhenTheStateMovesOn(t *testing.T) {
	ctx := context.Background()
	backup := func(h *idHub, hook func(n int)) (string, int, error) {
		n := 0
		out := filepath.Join(t.TempDir(), "b.tar.gz")
		_, err := Backup(ctx, h.dir, out, "dev", WithBackupSnapshotHook(func() { n++; hook(n) }))
		return out, n, err
	}
	t.Run("plain root sealed between copy and read", func(t *testing.T) {
		h := newIDHub(t, IdentityStatePlain)
		out, n, err := backup(h, func(n int) {
			if n == 1 { // the copy says plain; now the passphrase is set
				mustT(t, os.Remove(filepath.Join(h.dir, IdentityKeyFile)))
				h.root = bytes.Repeat([]byte{9}, 80)
				mustT(t, os.WriteFile(filepath.Join(h.dir, SealedRootFile), h.root, 0o600))
				h.sql(`UPDATE hub_identity SET root_sealed = 1, root_file_sha256 = ?`, sha(h.root))
			}
		})
		mustT(t, err)
		if n != 2 || identityNames(archiveNames(t, out)) != "root.key.age" {
			t.Fatalf("snapshots = %d, names = %v", n, archiveNames(t, out))
		}
	})
	t.Run("root rotation between copy and read", func(t *testing.T) {
		h := newIDHub(t, IdentityStateSealedNoCert)
		newRoot := bytes.Repeat([]byte{5}, 90)
		out, n, err := backup(h, func(n int) {
			if n == 1 {
				h.setRoot(t, SealedRootFile, newRoot)
			}
		})
		mustT(t, err)
		for _, e := range readArchive(t, out) {
			if e.hdr.Name == SealedRootFile && !bytes.Equal(e.body, newRoot) {
				t.Fatal("the archive holds the retired root")
			}
		}
		if n != 2 {
			t.Fatalf("snapshots = %d", n)
		}
	})
	t.Run("operational key replaced between copy and read", func(t *testing.T) {
		h := newIDHub(t, IdentityStateEstablished)
		out, n, err := backup(h, func(n int) {
			if n == 1 {
				h.issue(t, "")
			}
		})
		mustT(t, err)
		if n != 2 {
			t.Fatalf("snapshots = %d", n)
		}
		for _, e := range readArchive(t, out) {
			if e.hdr.Name == OpKeyFile && !bytes.Contains(e.body, []byte(readFile(t, filepath.Join(h.dir, OpKeyFile)))) {
				t.Fatal("op.key is not the current key")
			}
		}
	})
	t.Run("no matching file in five attempts", func(t *testing.T) {
		h := newIDHub(t, IdentityStateEstablished)
		mustT(t, os.Remove(filepath.Join(h.dir, OpKeyFile)))
		out, n, err := backup(h, func(int) {})
		if !errors.Is(err, ErrBackup) || n != maxSnapshotAttempts || maxSnapshotAttempts != 5 {
			t.Fatalf("err = %v after %d snapshots", err, n)
		}
		for _, want := range []string{"established", "op.key", "5 attempts"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
		if _, serr := os.Stat(out); serr == nil {
			t.Error("an archive was published")
		}
	})
	t.Run("sealed root already gone for a plain copy", func(t *testing.T) {
		h := newIDHub(t, IdentityStatePlain)
		mustT(t, os.Remove(filepath.Join(h.dir, IdentityKeyFile)))
		_, n, err := backup(h, func(int) {})
		if err == nil || n != 5 || !strings.Contains(err.Error(), IdentityKeyFile) {
			t.Fatalf("err = %v after %d snapshots", err, n)
		}
	})
}

func TestSnapshotChoosesFilesIndependentlyByWhatTheCopyRecords(t *testing.T) {
	ctx := context.Background()
	h := newIDHub(t, IdentityStateEstablished)
	// A rotation committed but not yet renamed: the copy records the staged
	// root and the staged operational key.
	oldRoot := h.root
	newRoot := bytes.Repeat([]byte{3}, 100)
	mustT(t, os.WriteFile(filepath.Join(h.dir, SealedRootNextFile), newRoot, 0o600))
	h.sql(`UPDATE hub_identity SET root_file_sha256 = ?`, sha(newRoot))
	oldOp := readFile(t, filepath.Join(h.dir, OpKeyFile))
	h.issue(t, OpKeyNextFile)
	mustT(t, os.WriteFile(filepath.Join(h.dir, OpKeyFile), []byte(oldOp), 0o600))
	n := 0
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	_, err := Backup(ctx, h.dir, out, "dev", WithBackupSnapshotHook(func() { n++ }))
	mustT(t, err)
	if n != 1 {
		t.Fatalf("snapshots = %d, want 1", n)
	}
	for _, e := range readArchive(t, out) {
		switch e.hdr.Name {
		case SealedRootFile:
			if !bytes.Equal(e.body, newRoot) || bytes.Equal(e.body, oldRoot) {
				t.Error("root.key.age is not the staged root recorded by the copy")
			}
		case OpKeyFile:
			if string(e.body) != readFile(t, filepath.Join(h.dir, OpKeyNextFile)) {
				t.Error("op.key is not the staged operational key of the certificate")
			}
		}
	}
	// And it restores as a consistent set.
	_, err = Restore(ctx, out, filepath.Join(t.TempDir(), "r"), false)
	mustT(t, err)
}

// subsetMigrations returns the embedded migrations below version max.
func subsetMigrations(t *testing.T, below string) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	ents, err := fs.ReadDir(hubMigrations, migrationsDir)
	mustT(t, err)
	for _, e := range ents {
		if e.Name() >= below {
			continue
		}
		b, err := fs.ReadFile(hubMigrations, migrationsDir+"/"+e.Name())
		mustT(t, err)
		m[migrationsDir+"/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	return m
}

// legacyArchive writes a format-1 (0.6.x) archive of a database migrated up to
// 0020. With identity it records an identity (and writes identity.key unless
// noKey).
func legacyArchive(t *testing.T, withIdentity, noKey bool) (archive string, m Manifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "old")
	s, err := Open(dir, WithMigrations(subsetMigrations(t, "0021"), migrationsDir))
	mustT(t, err)
	bodies := map[string][]byte{}
	if withIdentity {
		key, err := identity.GenerateKey()
		mustT(t, err)
		mustT(t, s.PutHubIdentity(context.Background(), HubIdentityRow{Pub: key.Public(), Fingerprint: key.Fingerprint().String(), CreatedAt: 1}))
		if !noKey {
			mustT(t, identity.SaveKey(filepath.Join(dir, IdentityKeyFile), key))
			bodies[IdentityKeyFile] = []byte(readFile(t, filepath.Join(dir, IdentityKeyFile)))
		}
	}
	mustT(t, s.PutSetting(context.Background(), "oidc_client_secret", "s3cret-value", true, "test"))
	mustT(t, s.Close())
	out := filepath.Join(t.TempDir(), "snap.db")
	_, err = vacuumCopy(context.Background(), filepath.Join(dir, DBFile), out)
	mustT(t, err)
	bodies[DBFile] = []byte(readFile(t, out))
	bodies[DataKeyFile] = []byte(readFile(t, filepath.Join(dir, DataKeyFile)))
	m = Manifest{Format: 1, Version: "0.6.3", SchemaVersion: 20, CreatedAt: "2026-01-01T00:00:00Z"}
	es := []entry{{hdr: regular(ManifestFile)}}
	for _, name := range archiveFiles {
		b, ok := bodies[name]
		if !ok {
			continue
		}
		m.Files = append(m.Files, ManifestEntry{Name: name, Size: int64(len(b)), SHA256: sumHex(b)})
		es = append(es, entry{hdr: regular(name), body: b})
	}
	es[0].body = manifestBody(t, m)
	archive = filepath.Join(t.TempDir(), "legacy.tar.gz")
	writeRaw(t, archive, es)
	return archive, m
}

func TestRestoreOfFormat1Archives(t *testing.T) {
	ctx := context.Background()
	t.Run("no identity restores as B0 and migrates", func(t *testing.T) {
		archive, m := legacyArchive(t, false, false)
		target := filepath.Join(t.TempDir(), "r")
		res, err := Restore(ctx, archive, target, false)
		mustT(t, err)
		if res.IdentityState != IdentityStateNone {
			t.Fatalf("state = %q", res.IdentityState)
		}
		// The transformed database is installed although its hash differs from
		// the manifest, and it is at the newest schema.
		var db ManifestEntry
		for _, e := range m.Files {
			if e.Name == DBFile {
				db = e
			}
		}
		got, err := os.ReadFile(filepath.Join(target, DBFile))
		mustT(t, err)
		if sumHex(got) == db.SHA256 {
			t.Fatal("the database was installed untransformed")
		}
		if v, err := schemaVersionOf(ctx, filepath.Join(target, DBFile)); err != nil || v < 21 {
			t.Fatalf("schema = %d, %v", v, err)
		}
		r, err := Open(target)
		mustT(t, err)
		defer r.Close()
		if rec, ok, _ := r.GetSetting(ctx, "oidc_client_secret"); !ok || rec.Value != "s3cret-value" {
			t.Fatal("the OIDC secret does not decrypt")
		}
	})
	t.Run("recorded identity with plain key restores as B1", func(t *testing.T) {
		archive, _ := legacyArchive(t, true, false)
		target := filepath.Join(t.TempDir(), "r")
		res, err := Restore(ctx, archive, target, false)
		mustT(t, err)
		if res.IdentityState != IdentityStatePlain {
			t.Fatalf("state = %q", res.IdentityState)
		}
		r, err := Open(target)
		mustT(t, err)
		defer r.Close()
		row, ok, err := r.HubIdentityV2(ctx)
		if err != nil || !ok || row.RootSealed || row.CurrentCert != nil || !row.MigratedFrom06 {
			t.Fatalf("row = %+v %v %v", row, ok, err)
		}
		if got := dirFiles(t, target); strings.Contains(strings.Join(got, ","), "op.key") {
			t.Fatalf("files = %v", got)
		}
	})
	t.Run("recorded identity without a key file is rejected", func(t *testing.T) {
		archive, _ := legacyArchive(t, true, true)
		target := filepath.Join(t.TempDir(), "r")
		_, err := Restore(ctx, archive, target, false)
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "no "+IdentityKeyFile) {
			t.Fatalf("err = %v", err)
		}
		assertAbsent(t, target)
	})
}

func TestRestoreRefusesIdentityFilesThatDoNotFitTheState(t *testing.T) {
	ctx := context.Background()
	other, err := identity.GenerateKey()
	mustT(t, err)
	otherKey := func() []byte {
		p := filepath.Join(t.TempDir(), "k")
		mustT(t, identity.SaveKey(p, other))
		return []byte(readFile(t, p))
	}()
	cases := []struct {
		state string
		mut   func(b map[string][]byte)
		want  string
	}{
		{IdentityStateNone, func(b map[string][]byte) { b[IdentityKeyFile] = otherKey }, "state"},
		{IdentityStatePlain, func(b map[string][]byte) { b[OpKeyFile] = otherKey }, "state"},
		{IdentityStatePlain, func(b map[string][]byte) { b[IdentityKeyFile] = otherKey }, "not the Hub identity"},
		{IdentityStateSealedNoCert, func(b map[string][]byte) { b[OpKeyFile] = otherKey }, "state"},
		{IdentityStateSealedNoCert, func(b map[string][]byte) { b[SealedRootFile] = []byte("tampered") }, "not the sealed root"},
		{IdentityStateSealedNoCert, func(b map[string][]byte) { delete(b, SealedRootFile) }, "state"},
		{IdentityStateEstablished, func(b map[string][]byte) { delete(b, OpKeyFile) }, "state"},
		{IdentityStateEstablished, func(b map[string][]byte) { b[OpKeyFile] = otherKey }, "not the operational key"},
		{IdentityStateEstablished, func(b map[string][]byte) { b[IdentityKeyFile] = otherKey }, "state"},
	}
	for _, c := range cases {
		h := newIDHub(t, c.state)
		good := filepath.Join(t.TempDir(), "good.tar.gz")
		_, err := Backup(ctx, h.dir, good, "dev")
		mustT(t, err)
		bad := rewriteArchive(t, good, c.mut)
		target := filepath.Join(t.TempDir(), "r")
		_, err = Restore(ctx, bad, target, false)
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.state, err, c.want)
		}
		assertAbsent(t, target)
	}
}

func TestRestoreRefusesAWrongManifestStateAndUnknownEntries(t *testing.T) {
	ctx := context.Background()
	h := newIDHub(t, IdentityStateEstablished)
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	_, err := Backup(ctx, h.dir, good, "dev")
	mustT(t, err)
	es := readArchive(t, good)
	var m Manifest
	mustT(t, json.Unmarshal(es[0].body, &m))
	write := func(mut func(m *Manifest, es *[]entry)) string {
		mm, ee := m, append([]entry(nil), es[1:]...)
		mm.Files = append([]ManifestEntry(nil), m.Files...)
		mut(&mm, &ee)
		all := append([]entry{{hdr: regular(ManifestFile), body: manifestBody(t, mm)}}, ee...)
		p := filepath.Join(t.TempDir(), "x.tar.gz")
		writeRaw(t, p, all)
		return p
	}
	cases := map[string]struct {
		p    string
		want string
	}{
		"manifest says another state": {write(func(m *Manifest, _ *[]entry) { m.IdentityState = IdentityStatePlain }), "identity state"},
		"unknown state":               {write(func(m *Manifest, _ *[]entry) { m.IdentityState = "hardened" }), "unknown identity state"},
		"another transition count":    {write(func(m *Manifest, _ *[]entry) { m.TransitionCount = 4 }), "transition"},
		"another fingerprint":         {write(func(m *Manifest, _ *[]entry) { m.RootFingerprint = "x" }), "fingerprint"},
		"entry not in the manifest": {write(func(_ *Manifest, e *[]entry) {
			*e = append(*e, entry{hdr: regular("root.key"), body: []byte("x")})
		}), "unexpected entry"},
		"manifest lists an unknown name": {write(func(m *Manifest, e *[]entry) {
			m.Files = append(m.Files, ManifestEntry{Name: "hub.cert", Size: 1, SHA256: sumHex([]byte("x"))})
			*e = append(*e, entry{hdr: regular("hub.cert"), body: []byte("x")})
		}), "unexpected file"},
		"format 1 listing a sealed root": {write(func(m *Manifest, _ *[]entry) { m.Format = 1 }), "unexpected file"},
	}
	for name, c := range cases {
		target := filepath.Join(t.TempDir(), "r")
		_, err := Restore(ctx, c.p, target, false)
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
		assertAbsent(t, target)
	}
}

func TestRestorePhasesAndInstallPlan(t *testing.T) {
	ctx := context.Background()
	t.Run("phase 1 files stay as the manifest says, phase 2 transforms a copy", func(t *testing.T) {
		archive, m := legacyArchive(t, true, false)
		t.Cleanup(func() { restorePhase2Hook = nil })
		restorePhase2Hook = func(verified, transformed string) {
			for _, e := range m.Files {
				if got := sumHex([]byte(readFile(t, filepath.Join(verified, e.Name)))); got != e.SHA256 {
					t.Errorf("verified %s was modified (%s)", e.Name, got)
				}
			}
			if sumHex([]byte(readFile(t, filepath.Join(transformed, DBFile)))) == m.Files[len(m.Files)-1].SHA256 {
				t.Error("phase 2 did not transform the database")
			}
			for _, s := range []string{"-wal", "-shm"} {
				if _, err := os.Stat(filepath.Join(transformed, DBFile+s)); err == nil {
					t.Errorf("phase 2 left %s%s", DBFile, s)
				}
			}
		}
		_, err := Restore(ctx, archive, filepath.Join(t.TempDir(), "r"), false)
		mustT(t, err)
	})
	t.Run("a file damaged after phase 2 is caught by the plan", func(t *testing.T) {
		h := newIDHub(t, IdentityStateEstablished)
		archive := filepath.Join(t.TempDir(), "b.tar.gz")
		_, err := Backup(ctx, h.dir, archive, "dev")
		mustT(t, err)
		t.Cleanup(func() { restorePhase2Hook = nil })
		restorePhase2Hook = func(_, transformed string) {
			f, err := os.OpenFile(filepath.Join(transformed, OpKeyFile), os.O_APPEND|os.O_WRONLY, 0)
			mustT(t, err)
			_, _ = f.WriteString("x")
			mustT(t, f.Close())
		}
		target := filepath.Join(t.TempDir(), "r")
		_, err = Restore(ctx, archive, target, false)
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), OpKeyFile) {
			t.Fatalf("err = %v", err)
		}
		assertAbsent(t, target)
	})
	t.Run("a staged file damaged before the install is caught and rolled back", func(t *testing.T) {
		h := newIDHub(t, IdentityStateSealedNoCert)
		archive := filepath.Join(t.TempDir(), "b.tar.gz")
		_, err := Backup(ctx, h.dir, archive, "dev")
		mustT(t, err)
		existing := markedHubDir(t, "old")
		orig := installRename
		t.Cleanup(func() { installRename = orig })
		done := false
		installRename = func(root *os.Root, from, to string) error {
			if !done && to == DataKeyFile && strings.Contains(from, ".restore-") && !strings.Contains(from, "/old/") {
				done = true
				stage := strings.SplitN(from, "/", 2)[0]
				f, err := root.OpenFile(stage+"/"+SealedRootFile, os.O_APPEND|os.O_WRONLY, 0)
				mustT(t, err)
				_, _ = f.WriteString("x")
				mustT(t, f.Close())
			}
			return orig(root, from, to)
		}
		_, err = Restore(ctx, archive, existing, true)
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "install plan") {
			t.Fatalf("err = %v", err)
		}
		if got := openMarked(t, existing); got != "old" {
			t.Fatalf("the existing Hub is %q after the failed restore", got)
		}
		if _, err := os.Stat(filepath.Join(existing, SealedRootFile)); err == nil {
			t.Error("a root file of the failed restore stayed")
		}
	})
}

func TestRestoreRootRollbackDetection(t *testing.T) {
	ctx := context.Background()
	h := newIDHub(t, IdentityStateEstablished)
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	_, err := Backup(ctx, h.dir, archive, "dev")
	mustT(t, err)

	// A Hub that already rotated away the root of the backup.
	rotated := newIDHub(t, IdentityStateEstablished)
	rotated.sql(`INSERT INTO hub_key_transitions (seq, old_pub, new_pub, created_at, sig_by_old) VALUES (1, ?, ?, 1, ?)`,
		h.signer.Public(), bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 64))
	mustT(t, rotated.s.Close())
	res, err := Restore(ctx, archive, rotated.dir, true)
	mustT(t, err)
	if !res.RootRolledBack {
		t.Fatal("no rollback detected over a Hub that rotated the backed-up root away")
	}
	if res.Transitions != 0 || res.RootFingerprint != h.mustFingerprint(t) {
		t.Fatalf("result = %+v", res)
	}
	// A fresh directory proves nothing.
	res, err = Restore(ctx, archive, filepath.Join(t.TempDir(), "r"), false)
	mustT(t, err)
	if res.RootRolledBack {
		t.Fatal("rollback claimed without evidence")
	}
}

// A longer chain alone is no evidence about the root of the backup.
func TestRestoreRootRollbackNeedsEvidenceTiedToTheBackupRoot(t *testing.T) {
	ctx := context.Background()
	h := newIDHub(t, IdentityStateEstablished)
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	_, err := Backup(ctx, h.dir, archive, "dev")
	mustT(t, err)
	other := newIDHub(t, IdentityStateEstablished) // another root, more transitions than the backup
	for i := 1; i <= 2; i++ {
		other.sql(`INSERT INTO hub_key_transitions (seq, old_pub, new_pub, created_at, sig_by_old) VALUES (?, ?, ?, 1, ?)`,
			i, bytes.Repeat([]byte{byte(10 + i)}, 32), bytes.Repeat([]byte{byte(20 + i)}, 32), bytes.Repeat([]byte{2}, 64))
	}
	mustT(t, other.s.Close())
	res, err := Restore(ctx, archive, other.dir, true)
	mustT(t, err)
	if res.RootRolledBack {
		t.Fatal("rollback claimed because the destination has more transitions")
	}
}

func (h *idHub) mustFingerprint(t *testing.T) string {
	t.Helper()
	f, err := identity.FingerprintOf(h.signer.Public())
	mustT(t, err)
	return f.String()
}

// rowDump renders every column of the hub_identity rows, so a column that a
// later migration adds is compared too.
func rowDump(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT * FROM hub_identity`)
	mustT(t, err)
	defer rows.Close()
	cols, _ := rows.Columns()
	var out strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		mustT(t, rows.Scan(ptrs...))
		for i, c := range cols {
			fmt.Fprintf(&out, "%s=%v;", c, vals[i])
		}
	}
	mustT(t, rows.Err())
	return out.String()
}

// mixedSelection backs up an established Hub whose root or operational key
// sits in the staged (.next) file only, and restores it.
func TestSnapshotMixedSelectionOfCurrentAndStagedFiles(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, h *idHub, wantRoot []byte, wantOp string) {
		t.Helper()
		out := filepath.Join(t.TempDir(), "b.tar.gz")
		_, err := Backup(ctx, h.dir, out, "dev")
		mustT(t, err)
		seen := 0
		for _, e := range readArchive(t, out) {
			switch e.hdr.Name {
			case SealedRootFile:
				seen++
				if !bytes.Equal(e.body, wantRoot) {
					t.Error("root.key.age is not the sealed root the copy records")
				}
			case OpKeyFile:
				seen++
				if string(e.body) != wantOp {
					t.Error("op.key is not the operational key of the certificate")
				}
			}
		}
		if seen != 2 {
			t.Fatalf("archive holds %d identity files", seen)
		}
		_, err = Restore(ctx, out, filepath.Join(t.TempDir(), "r"), false)
		mustT(t, err)
	}
	t.Run("root from root.key.age.next, operational key from op.key", func(t *testing.T) {
		h := newIDHub(t, IdentityStateEstablished)
		next := bytes.Repeat([]byte{3}, 100)
		mustT(t, os.WriteFile(filepath.Join(h.dir, SealedRootNextFile), next, 0o600))
		h.sql(`UPDATE hub_identity SET root_file_sha256 = ?`, sha(next))
		// op.key.next is a decoy that does not match the certificate.
		decoy, err := identity.GenerateKey()
		mustT(t, err)
		mustT(t, identity.SaveKey(filepath.Join(h.dir, OpKeyNextFile), decoy))
		check(t, h, next, readFile(t, filepath.Join(h.dir, OpKeyFile)))
	})
	t.Run("root from root.key.age, operational key from op.key.next", func(t *testing.T) {
		h := newIDHub(t, IdentityStateEstablished)
		oldOp := readFile(t, filepath.Join(h.dir, OpKeyFile))
		h.issue(t, OpKeyNextFile)
		mustT(t, os.WriteFile(filepath.Join(h.dir, OpKeyFile), []byte(oldOp), 0o600))
		// root.key.age.next is a decoy that does not match the recorded SHA-256.
		mustT(t, os.WriteFile(filepath.Join(h.dir, SealedRootNextFile), []byte("decoy"), 0o600))
		check(t, h, h.root, readFile(t, filepath.Join(h.dir, OpKeyNextFile)))
	})
}

// flipCertSignature returns an archive whose database certificate has one
// signature byte flipped.
func flipCertSignature(t *testing.T, good string) string {
	return rewriteArchive(t, good, func(b map[string][]byte) {
		p := filepath.Join(t.TempDir(), "x.db")
		mustT(t, os.WriteFile(p, b[DBFile], 0o600))
		db, err := sql.Open("sqlite", "file:"+p)
		mustT(t, err)
		defer func() { _ = db.Close() }()
		var cert []byte
		mustT(t, db.QueryRow(`SELECT current_cert FROM hub_identity`).Scan(&cert))
		cert[len(cert)-1] ^= 1
		_, err = db.Exec(`UPDATE hub_identity SET current_cert = ?`, cert)
		mustT(t, err)
		_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		mustT(t, err)
		_, err = db.Exec(`PRAGMA journal_mode = DELETE`)
		mustT(t, err)
		mustT(t, db.Close())
		nb, err := os.ReadFile(p)
		mustT(t, err)
		b[DBFile] = nb
	})
}

func TestRestoreRefusesACertificateWithABadSignature(t *testing.T) {
	ctx := context.Background()
	h := newIDHub(t, IdentityStateEstablished) // the certificate is long expired
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	_, err := Backup(ctx, h.dir, good, "dev")
	mustT(t, err)

	// An expired certificate with a valid signature restores.
	_, err = Restore(ctx, good, filepath.Join(t.TempDir(), "ok"), false)
	mustT(t, err)

	bad := flipCertSignature(t, good)
	hooked := false
	t.Cleanup(func() { restorePhase2Hook = nil })
	restorePhase2Hook = func(_, _ string) { hooked = true }
	target := filepath.Join(t.TempDir(), "r")
	_, err = Restore(ctx, bad, target, false)
	if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "valid signature") {
		t.Fatalf("err = %v", err)
	}
	if hooked {
		t.Error("the restore went past phase 2")
	}
	assertAbsent(t, target)

	// The same over an existing Hub: it is left as it was.
	existing := markedHubDir(t, "old")
	_, err = Restore(ctx, bad, existing, true)
	if !errors.Is(err, ErrBackup) {
		t.Fatalf("err = %v", err)
	}
	if got := openMarked(t, existing); got != "old" {
		t.Fatalf("the existing Hub is %q", got)
	}
}

func TestRejectedRestoresLeaveNothingInTheTempDir(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	h := newIDHub(t, IdentityStateEstablished)
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	_, err := Backup(ctx, h.dir, good, "dev")
	mustT(t, err)
	other, err := identity.GenerateKey()
	mustT(t, err)
	op := filepath.Join(t.TempDir(), "k")
	mustT(t, identity.SaveKey(op, other))
	otherKey := []byte(readFile(t, op))
	cases := map[string]string{
		"bad signature":     flipCertSignature(t, good),
		"wrong op key":      rewriteArchive(t, good, func(b map[string][]byte) { b[OpKeyFile] = otherKey }),
		"tampered root":     rewriteArchive(t, good, func(b map[string][]byte) { b[SealedRootFile] = []byte("tampered") }),
		"missing op key":    rewriteArchive(t, good, func(b map[string][]byte) { delete(b, OpKeyFile) }),
		"unusable key file": rewriteArchive(t, good, func(b map[string][]byte) { b[OpKeyFile] = []byte("not a key") }),
		"corrupt database":  rewriteArchive(t, good, func(b map[string][]byte) { b[DBFile] = []byte("not a database") }),
	}
	for name, p := range cases {
		target := filepath.Join(t.TempDir(), "r")
		if _, err := Restore(ctx, p, target, false); !errors.Is(err, ErrBackup) {
			t.Errorf("%s: err = %v", name, err)
		}
		assertAbsent(t, target)
		if got := dirFiles(t, tmp); len(got) != 0 {
			t.Errorf("%s: left %v in TMPDIR", name, got)
		}
		ents, err := os.ReadDir(tmp)
		mustT(t, err)
		if len(ents) != 0 {
			t.Errorf("%s: left %d entries in TMPDIR", name, len(ents))
		}
	}
}
