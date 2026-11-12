package hubid

import (
	"context"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// The 0.6 rotation recovery inside B1 (section 14, step 1).
func TestB1CompletesOrDiscardsAPending06Rotation(t *testing.T) {
	// The pending key is the recorded identity: the 0.6 rename is completed.
	e := newEnv(t)
	k1 := e.plain()
	k2, _ := identity.GenerateKey()
	if err := saveKey(e, PlainNextFile, k2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB().Exec(`UPDATE hub_identity SET pub = ?, fingerprint = ?`, []byte(k2.Public()), k2.Fingerprint().String()); err != nil {
		t.Fatal(err)
	}
	s := e.svc()
	if s.State() != StatePlain || e.has(PlainNextFile) || string(e.opPub(PlainFile)) != string(k2.Public()) {
		t.Fatal("the committed 0.6 rotation was not completed")
	}
	_ = k1
	// The pending key is not the recorded one: it never committed and is dropped.
	e2 := newEnv(t)
	k := e2.plain()
	stray, _ := identity.GenerateKey()
	_ = saveKey(e2, PlainNextFile, stray)
	s2 := e2.svc()
	if s2.State() != StatePlain || e2.has(PlainNextFile) || string(e2.opPub(PlainFile)) != string(k.Public()) {
		t.Fatal("the abandoned pending key was kept")
	}
}

// A plain key file next to no row (a 0.6 start interrupted between its two
// writes) is recorded as B1, as released behaviour; a pristine directory
// creates nothing at start.
func TestKeyFileWithoutARowIsRecordedAsB1(t *testing.T) {
	e := newEnv(t)
	k, _ := identity.GenerateKey()
	_ = saveKey(e, PlainFile, k)
	s := e.svc()
	if s.State() != StatePlain || e.row().Fingerprint != k.Fingerprint().String() || !e.row().MigratedFrom06 {
		t.Fatal("not recorded as B1")
	}
	e2 := newEnv(t)
	e2.svc()
	for _, f := range []string{PlainFile, RootFile, OpFile} {
		if e2.has(f) {
			t.Fatalf("start created %s", f)
		}
	}
	if _, ok, _ := e2.st.HubIdentityV2(context.Background()); ok {
		t.Fatal("start created a row")
	}
}

func TestStoreImplementsThePort(t *testing.T) {
	var _ Store = (*store.Store)(nil)
}
