//go:build unix

package client

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The API key file is a credential: a file that group or others can read or
// write is refused at construction and again on every request, whatever the
// key inside it is, and the refusal never repeats the key.
func TestHubAPIKeyFileMustBePrivate(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o666, 0o640, 0o604, 0o602, 0o660, 0o700 | 0o010} {
		f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
		if err := os.Chmod(f.keyFile, mode); err != nil {
			t.Fatal(err)
		}
		b, err := NewHub(f.cfg())
		if b != nil {
			b.Close()
		}
		if !errors.Is(err, ErrAPIKeyFile) {
			t.Errorf("mode %04o accepted at construction: %v", mode, err)
		} else if strings.Contains(err.Error(), f.key) {
			t.Errorf("mode %04o: the refusal repeats the key", mode)
		}
		if f.hits.Load() != 0 {
			t.Errorf("mode %04o: a request was made", mode)
		}
	}
}

// 0600 and 0400 stay legitimate. A file that becomes exposed after the backend
// was built is refused at the next request, before anything is sent, and a
// restored private file works again (the key is read per request).
func TestHubAPIKeyFileIsCheckedOnEveryRequest(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(f.keyFile, mode); err != nil {
			t.Fatal(err)
		}
		b, err := NewHub(f.cfg())
		if err != nil {
			t.Fatalf("legitimate mode %04o refused: %v", mode, err)
		}
		b.Close()
	}
	b := f.hub(t)
	ctx := context.Background()
	if _, err := b.Call(ctx, ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
	hits := f.hits.Load()
	for _, mode := range []os.FileMode{0o644, 0o666} {
		if err := os.Chmod(f.keyFile, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Call(ctx, ops.OpSessionsList, nil); !errors.Is(err, ErrAPIKeyFile) {
			t.Errorf("mode %04o at request time: %v", mode, err)
		}
		if _, err := b.Call(ctx, ops.OpSessionCreate, map[string]any{"request_id": "r"}); !errors.Is(err, ErrAPIKeyFile) {
			t.Errorf("write, mode %04o at request time: %v", mode, err)
		}
	}
	if f.hits.Load() != hits {
		t.Error("a request was sent with an exposed key file")
	}
	if err := os.Chmod(f.keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(ctx, ops.OpSessionsList, nil); err != nil {
		t.Errorf("the restored private file: %v", err)
	}
}

// A symbolic link is refused even when it points at a private file (the check
// is on the file that was opened, not on a path that can be swapped), and an
// atomic rotation (a new private file renamed over the old name, what
// identity.WritePrivate does) is used at once.
func TestHubAPIKeyFileSymlinkRefusedAndAtomicRotationUsed(t *testing.T) {
	var seen []string
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		jsonReply(w, 200, `{}`)
	})
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(f.keyFile, link); err != nil {
		t.Fatal(err)
	}
	cfg := f.cfg()
	cfg.APIKeyFile = link
	if b, err := NewHub(cfg); !errors.Is(err, ErrAPIKeyFile) {
		if b != nil {
			b.Close()
		}
		t.Errorf("a symbolic link to a private key file was accepted: %v", err)
	}

	b := f.hub(t)
	rotated := identity.NewAPIKey().String()
	if err := identity.WritePrivate(f.keyFile, []byte(rotated+"\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), ops.OpSessionsList, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "Bearer "+rotated {
		t.Errorf("the rotated key was not used at once")
	}
}

// A file owned by someone else cannot be tested without privileges; the
// ownership rule is that of identity.CheckPrivateFile, which the backend now
// uses, and is covered by the identity package. Here only the wiring is checked:
// the key file reader and the CA reader differ, and the CA file stays a plain
// (public) bounded file.
func TestHubCAFileNeedNotBePrivate(t *testing.T) {
	f := newHubFixture(t, func(f *hubFixture, w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, `{}`) })
	if err := os.Chmod(f.caFile, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := NewHub(f.cfg())
	if err != nil {
		t.Fatalf("a world-readable CA file (it is public) was refused: %v", err)
	}
	b.Close()
}
