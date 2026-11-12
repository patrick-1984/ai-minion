package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

func TestRestoreIdentityNotes(t *testing.T) {
	cases := []struct {
		name string
		res  store.RestoreResult
		want []string
		not  []string
	}{
		{"b0", store.RestoreResult{IdentityState: store.IdentityStateNone}, nil, []string{"rotated away", "Identity state"}},
		{"b1", store.RestoreResult{IdentityState: store.IdentityStatePlain, RootFingerprint: "FPR"}, []string{"plain", store.RootRollbackWarning}, nil},
		{"b3 advisory", store.RestoreResult{IdentityState: store.IdentityStateEstablished, RootFingerprint: "FPR", Transitions: 2},
			[]string{"established", "FPR", "2 master-key rotation", store.RootRollbackWarning, "passphrase of the backup's time"}, []string{"WARNING"}},
		{"b2 detected", store.RestoreResult{IdentityState: store.IdentityStateSealedNoCert, RootFingerprint: "FPR", RootRolledBack: true},
			[]string{"WARNING: " + store.RootRollbackWarning, "reset the affected Workers"}, nil},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if err := printIdentityNotes(&out, c.res); err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: output %q lacks %q", c.name, out.String(), w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(out.String(), n) {
				t.Errorf("%s: output %q has %q", c.name, out.String(), n)
			}
		}
	}
}

// A restore over a Hub whose chain leaves the root of the backup prints the
// rollback warning on the command's output.
func TestRunHubRestorePrintsTheRollbackWarning(t *testing.T) {
	ctx := context.Background()
	key, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	mkHub := func() string {
		dir := filepath.Join(t.TempDir(), "hub")
		s, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := identity.SaveKey(filepath.Join(dir, store.IdentityKeyFile), key); err != nil {
			t.Fatal(err)
		}
		if err := s.PutHubIdentity(ctx, store.HubIdentityRow{Pub: key.Public(), Fingerprint: key.Fingerprint().String(), CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		return dir
	}
	src := mkHub()
	archive := filepath.Join(t.TempDir(), "hub.tar.gz")
	if err := runHubBackup(ctx, &bytes.Buffer{}, hubBackupDeps{}, src, archive, plainBackup); err != nil {
		t.Fatal(err)
	}
	// The destination followed a rotation away from the root of the backup.
	dst := mkHub()
	s, err := store.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	newPub := bytes.Repeat([]byte{9}, 32)
	err = s.RotateHubIdentity(ctx, store.RotateParams{
		Expect:     key.Public(),
		Transition: store.TransitionRow{Seq: 1, OldPub: key.Public(), NewPub: newPub, CreatedAt: 2, SigByOld: bytes.Repeat([]byte{1}, 64)},
		Identity:   store.HubIdentityRow{Pub: newPub, Fingerprint: "fp", CreatedAt: 2},
	})
	_ = s.Close()
	if err != nil {
		t.Fatal(err)
	}
	hostOnly := hubBackupDeps{containerDir: func(string) containerKind { return notContainer }}
	var out bytes.Buffer
	if err := runHubRestore(ctx, &out, hostOnly, dst, archive, true, "", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING: "+store.RootRollbackWarning) {
		t.Fatalf("output lacks the rollback warning: %q", out.String())
	}
}
