package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// identityHub is a Hub directory whose database records the identity of a
// real key saved as identity.key.
func identityHub(t *testing.T) (dir string, key *identity.Key) {
	t.Helper()
	dir = newHubDir(t)
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, IdentityKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := identity.SaveKey(filepath.Join(dir, IdentityKeyFile), key); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutHubIdentity(context.Background(), HubIdentityRow{Pub: key.Public(), Fingerprint: key.Fingerprint().String(), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	return dir, key
}

// rewriteArchive rebuilds a plain archive after mut changed its bodies; the
// manifest follows the bodies. A nil body removes the entry.
func rewriteArchive(t *testing.T, src string, mut func(bodies map[string][]byte)) string {
	t.Helper()
	es := readArchive(t, src)
	var m Manifest
	if err := json.Unmarshal(es[0].body, &m); err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{}
	for _, e := range es[1:] {
		bodies[e.hdr.Name] = e.body
	}
	mut(bodies)
	out := []entry{{hdr: regular(ManifestFile)}}
	m.Files = nil
	for _, name := range archiveFiles {
		b, ok := bodies[name]
		if !ok || b == nil {
			continue
		}
		m.Files = append(m.Files, ManifestEntry{Name: name, Size: int64(len(b)), SHA256: sumHex(b)})
		out = append(out, entry{hdr: regular(name), body: b})
	}
	out[0].body = manifestBody(t, m)
	p := filepath.Join(t.TempDir(), "rewritten.tar.gz")
	writeRaw(t, p, out)
	return p
}

func assertAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("%s exists (err %v)", p, err)
	}
}

func TestRestoreRefusesAnArchiveWithoutTheRecordedIdentityKey(t *testing.T) {
	ctx := context.Background()
	dir, _ := identityHub(t)
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	if _, err := Backup(ctx, dir, good, "dev"); err != nil {
		t.Fatal(err)
	}
	// Control: the intact archive restores.
	if _, err := Restore(ctx, good, filepath.Join(t.TempDir(), "ok"), false); err != nil {
		t.Fatal(err)
	}
	bad := rewriteArchive(t, good, func(b map[string][]byte) { delete(b, IdentityKeyFile) })

	fresh := filepath.Join(t.TempDir(), "x", "hub")
	_, err := Restore(ctx, bad, fresh, false)
	if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "no "+IdentityKeyFile) {
		t.Fatalf("err = %v", err)
	}
	assertAbsent(t, fresh)

	// An existing Hub stays as it is, also with --force.
	existing := markedHubDir(t, "old")
	if _, err := Restore(ctx, bad, existing, true); !errors.Is(err, ErrBackup) {
		t.Fatalf("force: %v", err)
	}
	if got := openMarked(t, existing); got != "old" {
		t.Fatalf("the existing Hub was changed: %q", got)
	}
}

func TestRestoreRefusesAMismatchedIdentityKey(t *testing.T) {
	ctx := context.Background()
	dir, _ := identityHub(t)
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	if _, err := Backup(ctx, dir, good, "dev"); err != nil {
		t.Fatal(err)
	}
	other, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "other.key")
	if err := identity.SaveKey(tmp, other); err != nil {
		t.Fatal(err)
	}
	otherBytes := []byte(readFile(t, tmp))
	for name, body := range map[string][]byte{"other key": otherBytes, "garbage": []byte("garbage")} {
		bad := rewriteArchive(t, good, func(b map[string][]byte) { b[IdentityKeyFile] = body })
		existing := markedHubDir(t, "old")
		_, err := Restore(ctx, bad, existing, true)
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), IdentityKeyFile) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if got := openMarked(t, existing); got != "old" {
			t.Fatalf("%s: the existing Hub was changed: %q", name, got)
		}
		fresh := filepath.Join(t.TempDir(), "hub")
		if _, err := Restore(ctx, bad, fresh, false); err == nil {
			t.Fatalf("%s: restored", name)
		}
		assertAbsent(t, fresh)
	}
}

// craftedAgeArchive returns an age file whose scrypt stanza asks for the work
// factor logN. The rest is random-looking filler: a decryptor that does not
// refuse the factor first would run scrypt before noticing.
func craftedAgeArchive(t *testing.T, logN string) string {
	t.Helper()
	salt := strings.Repeat("A", 22)
	body := strings.Repeat("A", 43)
	mac := strings.Repeat("A", 43)
	content := ageMagic + "-> scrypt " + salt + " " + logN + "\n" + body + "\n--- " + mac + "\n" + strings.Repeat("\x01", 64)
	p := filepath.Join(t.TempDir(), "crafted.age")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestoreRefusesAnExcessiveScryptWorkFactorWithoutRunningIt(t *testing.T) {
	p := craftedAgeArchive(t, "22")
	target := filepath.Join(t.TempDir(), "hub")
	start := time.Now()
	_, err := Restore(context.Background(), p, target, false, WithRestorePassphrase("pw"))
	elapsed := time.Since(start)
	if !errors.Is(err, ErrBackup) || errors.Is(err, ErrWrongPassphrase) || !strings.Contains(err.Error(), "work factor") {
		t.Fatalf("err = %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v: the key derivation ran", elapsed)
	}
	assertAbsent(t, target)
}

func TestRestoreRefusesDeclaredSizesOfSmallFiles(t *testing.T) {
	ctx := context.Background()
	dir, _ := identityHub(t)
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	if _, err := Backup(ctx, dir, good, "dev"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(b map[string][]byte){
		"data key too long": func(b map[string][]byte) { b[DataKeyFile] = bytes.Repeat([]byte{1}, KeySize+1) },
		"identity too long": func(b map[string][]byte) { b[IdentityKeyFile] = bytes.Repeat([]byte{1}, 5000) },
		"tls key too long":  func(b map[string][]byte) { b[tlsKeyFile] = bytes.Repeat([]byte{1}, 65<<10) },
		"tls cert too long": func(b map[string][]byte) { b[tlsCertFile] = bytes.Repeat([]byte{1}, 1<<20+1) },
	}
	for name, mut := range cases {
		bad := rewriteArchive(t, good, mut)
		fresh := filepath.Join(t.TempDir(), "hub")
		_, err := Restore(ctx, bad, fresh, false)
		if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "manifest") {
			t.Errorf("%s: err = %v (want a refusal from the manifest sizes)", name, err)
		}
		assertAbsent(t, fresh)
	}
}

func TestRestoreRefusesAnHugeDeclaredSizeBeforeExtracting(t *testing.T) {
	ctx := context.Background()
	m, bodies := validParts(t)
	m.Files[1].Size = 1 << 50 // the database
	es := []entry{{hdr: regular(ManifestFile), body: manifestBody(t, m)}}
	for _, f := range m.Files {
		es = append(es, entry{hdr: regular(f.Name), body: bodies[f.Name]})
	}
	p := filepath.Join(t.TempDir(), "huge.tar.gz")
	writeRaw(t, p, es)
	_, err := Restore(ctx, p, filepath.Join(t.TempDir(), "hub"), false)
	if !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreEnforcesTheTotalSizeLimit(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	plain := filepath.Join(t.TempDir(), "p.tar.gz")
	m, err := Backup(ctx, dir, plain, "dev")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	target := filepath.Join(t.TempDir(), "hub")
	if _, err := Restore(ctx, plain, target, false, WithRestoreLimit(total-1)); !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("plain: %v", err)
	}
	assertAbsent(t, target)
	if _, err := Restore(ctx, plain, target, false, WithRestoreLimit(total)); err != nil {
		t.Fatalf("a limit equal to the size must pass: %v", err)
	}

	// The decrypted stream is bounded too.
	enc := encryptedBackup(t, dir, "pw")
	target2 := filepath.Join(t.TempDir(), "hub")
	if _, err := Restore(ctx, enc, target2, false, WithRestorePassphrase("pw"), WithRestoreLimit(1024)); !errors.Is(err, ErrBackup) || !errors.Is(err, errDecryptedTooLarge) {
		t.Fatalf("encrypted: %v", err)
	}
	assertAbsent(t, target2)
}

type cancelAfterFirstRead struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (c *cancelAfterFirstRead) Read(p []byte) (int, error) {
	n, err := c.r.Read(p[:1])
	c.cancel()
	return n, err
}

func TestCopyStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &cancelAfterFirstRead{r: bytes.NewReader(make([]byte, 1<<20)), cancel: cancel}
	n, _, err := copyCtx(ctx, io.Discard, src, 1<<30)
	if !errors.Is(err, context.Canceled) || n >= 1<<20 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestRestoreWithACancelledContextStopsAndLeavesNothing(t *testing.T) {
	dir := newHubDir(t)
	plain := filepath.Join(t.TempDir(), "p.tar.gz")
	if _, err := Backup(context.Background(), dir, plain, "dev"); err != nil {
		t.Fatal(err)
	}
	enc := encryptedBackup(t, dir, "pw")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, args := range map[string][]Option{"plain": nil, "encrypted": {WithRestorePassphrase("pw")}} {
		archive := plain
		if name == "encrypted" {
			archive = enc
		}
		target := filepath.Join(t.TempDir(), "hub")
		_, err := Restore(ctx, archive, target, false, args...)
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrBackup) {
			t.Errorf("%s: err = %v", name, err)
		}
		assertAbsent(t, target)
	}
}

// gzipTamper returns the plain archive at src with edit applied to its bytes.
func gzipTamper(t *testing.T, src string, edit func(b []byte) []byte) []byte {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	return edit(append([]byte(nil), raw...))
}

func TestRestoreRequiresAVerifiedGzipEnd(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	if _, err := Backup(ctx, dir, good, "dev"); err != nil {
		t.Fatal(err)
	}
	variants := map[string][]byte{
		"bad crc":         gzipTamper(t, good, func(b []byte) []byte { b[len(b)-8] ^= 0xff; return b }),
		"bad length":      gzipTamper(t, good, func(b []byte) []byte { b[len(b)-1] ^= 0xff; return b }),
		"missing trailer": gzipTamper(t, good, func(b []byte) []byte { return b[:len(b)-8] }),
	}
	for name, body := range variants {
		// Plain.
		p := filepath.Join(t.TempDir(), "bad.tar.gz")
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "hub")
		if _, err := Restore(ctx, p, target, false); !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "gzip") {
			t.Errorf("plain %s: err = %v", name, err)
		}
		assertAbsent(t, target)

		// The same stream inside an encrypted archive.
		encPath := filepath.Join(t.TempDir(), "bad.age")
		f, err := os.Create(encPath)
		if err != nil {
			t.Fatal(err)
		}
		rcp, _ := age.NewScryptRecipient("pw")
		rcp.SetWorkFactor(scryptWorkFactor)
		w, err := age.Encrypt(f, rcp)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
		_ = w.Close()
		_ = f.Close()
		target2 := filepath.Join(t.TempDir(), "hub")
		if _, err := Restore(ctx, encPath, target2, false, WithRestorePassphrase("pw")); !errors.Is(err, ErrBackup) || !strings.Contains(err.Error(), "gzip") {
			t.Errorf("encrypted %s: err = %v", name, err)
		}
		assertAbsent(t, target2)
	}
}

func TestARejectedRestoreLeavesAnAbsentDestinationAbsent(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	good := filepath.Join(t.TempDir(), "good.tar.gz")
	if _, err := Backup(ctx, dir, good, "dev"); err != nil {
		t.Fatal(err)
	}
	bad := rewriteArchive(t, good, func(b map[string][]byte) { b[DBFile] = []byte("not a database, but a manifest-valid file") })
	parent := t.TempDir()
	target := filepath.Join(parent, "a", "b", "hub")
	if _, err := Restore(ctx, bad, target, false); !errors.Is(err, ErrBackup) {
		t.Fatalf("err = %v", err)
	}
	assertAbsent(t, target)
	assertAbsent(t, filepath.Join(parent, "a"))
}

// paxBlock returns one tar extension header ('x') carrying a single PAX record
// of about size bytes, built by hand because tar.Writer folds extension
// headers into the entry that follows.
func paxBlock(size int) []byte {
	val := strings.Repeat("a", size)
	rec := fmt.Sprintf("comment=%s\n", val)
	n := len(rec) + 1
	for len(fmt.Sprintf("%d ", n))+len(rec) != n {
		n = len(fmt.Sprintf("%d ", n)) + len(rec)
	}
	body := []byte(fmt.Sprintf("%d %s", n, rec))
	var h [512]byte
	copy(h[0:], "PaxHeader")
	copy(h[100:], "0000600\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", len(body)))
	copy(h[136:], "00000000000\x00")
	h[156] = 'x'
	copy(h[257:], "ustar\x0000")
	copy(h[148:], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	out := append(h[:], body...)
	if pad := len(body) % 512; pad != 0 {
		out = append(out, make([]byte, 512-pad)...)
	}
	return out
}

// paxFloodArchive is a valid archive with many PAX headers before the manifest.
func paxFloodArchive(t *testing.T, count, size int) string {
	t.Helper()
	m, bodies := validParts(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for i := 0; i < count; i++ {
		if _, err := gz.Write(paxBlock(size)); err != nil {
			t.Fatal(err)
		}
	}
	tw := tar.NewWriter(gz)
	put := func(name string, body []byte) {
		h := regular(name)
		h.Size, h.Mode = int64(len(body)), 0o600
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	put(ManifestFile, manifestBody(t, m))
	for _, f := range m.Files {
		put(f.Name, bodies[f.Name])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "pax.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestoreCountsExtensionHeadersAgainstTheBudget(t *testing.T) {
	// 100 headers of about 60 KiB are about 6 MiB, over limit + allowance.
	p := paxFloodArchive(t, 100, 60000)
	target := filepath.Join(t.TempDir(), "hub")
	_, err := Restore(context.Background(), p, target, false, WithRestoreLimit(1024))
	if !errors.Is(err, ErrBackup) || !errors.Is(err, errArchiveBudget) {
		t.Fatalf("err = %v", err)
	}
	assertAbsent(t, target)

	// A few headers stay inside the budget and the archive restores.
	ok := paxFloodArchive(t, 2, 1000)
	if _, err := Restore(context.Background(), ok, filepath.Join(t.TempDir(), "hub"), false); err != nil {
		t.Fatalf("small extension headers must pass: %v", err)
	}
}

func TestRestoreStopsOnACancelledContextWhileReadingExtensionHeaders(t *testing.T) {
	p := paxFloodArchive(t, 100, 60000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(t.TempDir(), "hub")
	_, err := Restore(ctx, p, target, false)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrBackup) {
		t.Fatalf("err = %v", err)
	}
	assertAbsent(t, target)
}

func TestRestoreRefusesADecryptedStreamOverTheLimitWithSmallManifestTotals(t *testing.T) {
	ctx := context.Background()
	dir := newHubDir(t)
	plain := filepath.Join(t.TempDir(), "p.tar.gz")
	m, err := Backup(ctx, dir, plain, "dev")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	// Re-compress the tar without compression: the age plaintext is then at
	// least as big as the file content plus tar headers, over the limit,
	// while the manifest totals equal it.
	f, err := os.Open(plain)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&raw, gzip.NoCompression)
	if _, err := io.Copy(zw, zr); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if int64(raw.Len()) <= total {
		t.Fatalf("test setup: plaintext %d not over %d", raw.Len(), total)
	}
	var enc bytes.Buffer
	rcp, err := age.NewScryptRecipient("pw")
	if err != nil {
		t.Fatal(err)
	}
	rcp.SetWorkFactor(scryptWorkFactor)
	w, err := age.Encrypt(&enc, rcp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "big.age")
	if err := os.WriteFile(archive, enc.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "hub")
	_, err = Restore(ctx, archive, target, false, WithRestorePassphrase("pw"), WithRestoreLimit(total))
	if !errors.Is(err, ErrBackup) || !errors.Is(err, errDecryptedTooLarge) {
		t.Fatalf("err = %v", err)
	}
	assertAbsent(t, target)
	// With room for the plaintext the same file restores.
	if _, err := Restore(ctx, archive, filepath.Join(t.TempDir(), "hub"), false, WithRestorePassphrase("pw"), WithRestoreLimit(int64(raw.Len()))); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
}

func TestFailedRestoreRemovesTheAncestorDirectoriesItCreated(t *testing.T) {
	dir := newHubDir(t)
	plain := filepath.Join(t.TempDir(), "p.tar.gz")
	if _, err := Backup(context.Background(), dir, plain, "dev"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "hub")
	restoreDirsHook = func(string) error { return fmt.Errorf("%w: injected failure after directory creation", ErrBackup) }
	defer func() { restoreDirsHook = nil }()
	if _, err := Restore(context.Background(), plain, target, false); err == nil {
		t.Fatal("expected the injected failure")
	}
	assertAbsent(t, filepath.Join(root, "a"))

	// Existing ancestors and their content are kept.
	keep := filepath.Join(root, "keep")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), plain, filepath.Join(keep, "x", "hub"), false); err == nil {
		t.Fatal("expected the injected failure")
	}
	assertAbsent(t, filepath.Join(keep, "x"))
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("pre-existing directory removed: %v", err)
	}
}
