package version

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// testTUF is a private TUF repository that publishes one target,
// trusted_root.json. Root version 1 is the trust anchor; with rotate the
// repository also has root version 2, which replaces every key, and signs
// timestamp, snapshot and targets with the new keys.
type testTUF struct {
	anchor      []byte
	roots       map[int][]byte
	timestamp   []byte
	snapshot    []byte
	targets     []byte
	trustedRoot []byte
}

func tufSigner(t testing.TB) (signature.Signer, *metadata.Key) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := signature.LoadSigner(priv, crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	k, err := metadata.KeyFromPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return s, k
}

func tufSign[T metadata.Roles](t testing.TB, m *metadata.Metadata[T], signers ...signature.Signer) []byte {
	t.Helper()
	for _, s := range signers {
		if _, err := m.Sign(s); err != nil {
			t.Fatal(err)
		}
	}
	b, err := m.ToBytes(false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newTestTUF builds a repository around trustedRoot. Metadata expires at
// expires, which tests set in the past to model a long-offline cache.
//
// The published target is trustedRoot pretty-printed, the shape the public
// Sigstore repository serves. It is deliberately not what protojson would
// emit, so a cache written from a re-encoded root fails the signed length and
// hash check, whatever the build.
func newTestTUF(t testing.TB, trustedRoot []byte, rotate bool, expires time.Time) *testTUF {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, trustedRoot, "", "  "); err != nil {
		t.Fatal(err)
	}
	trustedRoot = append(pretty.Bytes(), '\n')
	r := &testTUF{roots: map[int][]byte{}, trustedRoot: trustedRoot}
	mkRoot := func(version int64, keys map[string]signature.Signer) (*metadata.Metadata[metadata.RootType], map[string]signature.Signer) {
		m := metadata.Root(expires)
		m.Signed.Version = version
		for role, s := range keys {
			pub, err := s.PublicKey()
			if err != nil {
				t.Fatal(err)
			}
			k, err := metadata.KeyFromPublicKey(pub)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Signed.AddKey(k, role); err != nil {
				t.Fatal(err)
			}
		}
		return m, keys
	}
	newKeys := func() map[string]signature.Signer {
		out := map[string]signature.Signer{}
		for _, role := range []string{metadata.ROOT, metadata.TIMESTAMP, metadata.SNAPSHOT, metadata.TARGETS} {
			s, _ := tufSigner(t)
			out[role] = s
		}
		return out
	}
	keys1 := newKeys()
	root1, _ := mkRoot(1, keys1)
	r.anchor = tufSign(t, root1, keys1[metadata.ROOT])
	r.roots[1] = r.anchor
	cur := keys1
	if rotate {
		keys2 := newKeys()
		root2, _ := mkRoot(2, keys2)
		r.roots[2] = tufSign(t, root2, keys1[metadata.ROOT], keys2[metadata.ROOT])
		cur = keys2
	}
	tf, err := metadata.TargetFile().FromBytes(trustedRootFile, trustedRoot, "sha256")
	if err != nil {
		t.Fatal(err)
	}
	tm := metadata.Targets(expires)
	tm.Signed.Targets[trustedRootFile] = tf
	r.targets = tufSign(t, tm, cur[metadata.TARGETS])
	sm := metadata.Snapshot(expires)
	sm.Signed.Meta["targets.json"] = &metadata.MetaFiles{Version: 1}
	r.snapshot = tufSign(t, sm, cur[metadata.SNAPSHOT])
	tsm := metadata.Timestamp(expires)
	tsm.Signed.Meta["snapshot.json"] = &metadata.MetaFiles{Version: 1}
	r.timestamp = tufSign(t, tsm, cur[metadata.TIMESTAMP])
	return r
}

var tufRootPath = regexp.MustCompile(`/([0-9]+)\.root\.json$`)

// serve publishes the repository over loopback http.
func (r *testTUF) serve(t testing.TB) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		var data []byte
		switch {
		case tufRootPath.MatchString(p):
			var n int
			_, _ = fmt.Sscan(tufRootPath.FindStringSubmatch(p)[1], &n)
			data = r.roots[n]
		case strings.HasSuffix(p, "timestamp.json"):
			data = r.timestamp
		case strings.HasSuffix(p, "snapshot.json"):
			data = r.snapshot
		case strings.HasSuffix(p, "targets.json"):
			data = r.targets
		case strings.HasSuffix(p, trustedRootFile):
			data = r.trustedRoot
		}
		if data == nil {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// install writes the repository into dir the way an online fetch leaves it
// (the cache layout of TUFTrustedRoot), without any network.
func (r *testTUF) install(t testing.TB, dir, repoURL string) {
	t.Helper()
	meta := filepath.Join(dir, "tuf", tuf.URLToPath(repoURL))
	for name, data := range map[string][]byte{
		filepath.Join(meta, "timestamp.json"): r.timestamp,
		filepath.Join(meta, "snapshot.json"):  r.snapshot,
		filepath.Join(meta, "targets.json"):   r.targets,
		filepath.Join(dir, trustedRootFile):   r.trustedRoot,
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for n, data := range r.roots {
		if n == 1 {
			continue
		}
		if err := os.MkdirAll(filepath.Join(dir, tufRootsDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, tufRootsDir, fmt.Sprintf("%d.root.json", n)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
