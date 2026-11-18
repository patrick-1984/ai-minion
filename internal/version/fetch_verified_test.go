package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func (e *env) fetch(selector string) (Fetched, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.updater.FetchVerified(ctx, mustSelector(e.t, selector), testOS, testArch)
}

// FetchVerified gets the signed release of one version into the cache and
// installs nothing: the Windows service install copies it from there.
func TestFetchVerifiedReturnsTheVerifiedExecutable(t *testing.T) {
	e := newEnv(t, standardHost(t))
	f, err := e.fetch("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, f.Path)
	sum := sha256.Sum256([]byte(got))
	if f.Tag != "v1.2.3" || got != "minion v1.2.3" || f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("fetched %+v with content %q", f, got)
	}
	if e.installed() != "installed binary" {
		t.Error("FetchVerified replaced the installed binary")
	}
	// Again, with the entry already cached.
	if f2, err := e.fetch("v1.2.3"); err != nil || f2.SHA256 != f.SHA256 {
		t.Fatalf("second fetch: %+v, %v", f2, err)
	}
}

func TestFetchVerifiedRefusesATamperedArchive(t *testing.T) {
	e := newEnv(t, standardHost(t))
	e.host.setTamper(ArchiveName("v1.2.3", testOS, testArch, ""), func(_ string, data []byte) []byte {
		return append(append([]byte(nil), data...), 0)
	})
	if _, err := e.fetch("v1.2.3"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	assertNoCacheEntry(t, e, "v1.2.3")
}

func TestFetchVerifiedNeedsAVerifierAndASource(t *testing.T) {
	sel := mustSelector(t, "v1.2.3")
	if _, err := (&Updater{}).FetchVerified(context.Background(), sel, testOS, testArch); err == nil {
		t.Error("no verifier accepted")
	}
	u := &Updater{Verifier: func(context.Context, bool) (BlobVerifier, error) { return nil, nil }}
	if _, err := u.FetchVerified(context.Background(), sel, testOS, testArch); err == nil {
		t.Error("no source accepted")
	}
}
