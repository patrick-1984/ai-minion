package version

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
	"github.com/theupdateframework/go-tuf/v2/metadata/trustedmetadata"
)

// TrustedRootProvider supplies the Sigstore trusted material. Online it may
// refresh it; offline it must not touch the network.
type TrustedRootProvider interface {
	TrustedMaterial(ctx context.Context, offline bool) (root.TrustedMaterial, error)
}

// StaticTrustedRoot is a [TrustedRootProvider] over fixed material. It serves
// tests and private Sigstore deployments.
type StaticTrustedRoot struct{ Material root.TrustedMaterial }

// TrustedMaterial implements [TrustedRootProvider].
func (s StaticTrustedRoot) TrustedMaterial(context.Context, bool) (root.TrustedMaterial, error) {
	if s.Material == nil {
		return nil, errors.New("no trusted root configured")
	}
	return s.Material, nil
}

// TUFTrustedRoot fetches the public-good Sigstore trusted root through TUF
// (sigstore-go embeds the TUF root of trust) and keeps what it needs in Dir,
// so that an offline run can still verify a cached release. Fetching happens
// only in an online "minion update".
//
// The cache is never trusted by itself. An offline run authenticates the
// cached trusted_root.json against the embedded TUF trust anchor: the cached
// root metadata chain, timestamp, snapshot and targets must carry valid
// signatures from the keys the anchor (and its signed successors) names, and
// the cached trusted_root.json must match the length and hashes the signed
// targets metadata states. Without that, anyone who can write the cache could
// swap in Fulcio and Rekor roots of their own and forge a matching release.
// Offline, TUF expiry is not enforced: there is no way to refresh, so the
// cache proves authenticity, not freshness.
//
// This is the second outbound destination of an online update, next to the
// release host: the public Sigstore TUF repository, tuf-repo-cdn.sigstore.dev
// (the host AD-19 and NFR8 do not name). An offline update never contacts it.
// Every request carries the context of the update and a timeout.
type TUFTrustedRoot struct {
	// Dir holds the TUF cache and trusted_root.json. It is created on use.
	Dir string
	// RepositoryURL overrides the TUF repository (https, or http on
	// loopback). Empty means the public Sigstore repository. It exists for
	// tests and private Sigstore deployments.
	RepositoryURL string
	// Root overrides the embedded TUF trust anchor (root.json). Empty means
	// the embedded public-good root. It exists for tests and private
	// Sigstore deployments, together with RepositoryURL.
	Root []byte
}

const (
	trustedRootFile = "trusted_root.json"
	// tufRootsDir keeps every root metadata version the last online fetch
	// downloaded, so that an offline run can walk the root chain from the
	// embedded anchor to the current root. The files are candidates only:
	// each one is verified against its predecessor before it is used.
	tufRootsDir = "tuf-roots"
)

func (t TUFTrustedRoot) anchor() []byte {
	if len(t.Root) > 0 {
		return t.Root
	}
	return tuf.DefaultRoot()
}

func (t TUFTrustedRoot) repositoryURL() string {
	if t.RepositoryURL != "" {
		return t.RepositoryURL
	}
	return tuf.DefaultMirror
}

// TrustedMaterial implements [TrustedRootProvider].
func (t TUFTrustedRoot) TrustedMaterial(ctx context.Context, offline bool) (root.TrustedMaterial, error) {
	if t.Dir == "" {
		return nil, errors.New("no directory for the trusted root cache")
	}
	copyPath := filepath.Join(t.Dir, trustedRootFile)
	if offline {
		data, err := t.authenticatedCachedRoot(copyPath)
		if err != nil {
			return nil, fmt.Errorf("no usable cached Sigstore trusted root (run minion update once while online): %w", err)
		}
		tr, err := root.NewTrustedRootFromJSON(data)
		if err != nil {
			return nil, fmt.Errorf("no usable cached Sigstore trusted root (run minion update once while online): %w", err)
		}
		return tr, nil
	}
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: cannot fetch the Sigstore trusted root: %w", ErrNetwork, err)
	}
	rec := &rootRecorder{Fetcher: newTUFFetcher(ctx)}
	opts := tuf.DefaultOptions().
		WithContext(ctx).
		WithFetcher(rec).
		WithRoot(t.anchor()).
		WithCachePath(filepath.Join(t.Dir, "tuf"))
	if t.RepositoryURL != "" {
		if err := checkURL(t.RepositoryURL); err != nil {
			return nil, fmt.Errorf("the Sigstore TUF repository URL: %w", err)
		}
		opts = opts.WithRepositoryBaseURL(t.RepositoryURL)
	}
	client, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot fetch the Sigstore trusted root: %w", ErrNetwork, err)
	}
	// raw are the exact bytes the signed TUF targets metadata vouches for
	// (GetTarget checks length and hashes). They, and never a re-encoding
	// of the parsed root, are what the offline cache must hold: the offline
	// check compares them with the signed length and hash.
	raw, err := client.GetTarget(trustedRootFile)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot fetch the Sigstore trusted root: %w", ErrNetwork, err)
	}
	tr, err := root.NewTrustedRootFromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("the Sigstore trusted root: %w", err)
	}
	// Best effort: a failed copy only costs the offline fallback.
	rec.save(filepath.Join(t.Dir, tufRootsDir))
	_ = writeFileAtomic(copyPath, raw, 0o600)
	return tr, nil
}

// authenticatedCachedRoot returns the bytes of the cached trusted_root.json
// after proving that signed TUF metadata, rooted in the trust anchor, vouches
// for exactly these bytes. It never touches the network.
func (t TUFTrustedRoot) authenticatedCachedRoot(copyPath string) ([]byte, error) {
	data, err := os.ReadFile(copyPath)
	if err != nil {
		return nil, err
	}
	tm, err := trustedmetadata.New(t.anchor())
	if err != nil {
		return nil, fmt.Errorf("the embedded TUF trust anchor: %w", err)
	}
	// Offline there is nothing to refresh from: check signatures, versions
	// and hashes, not expiry.
	tm.RefTime = time.Unix(0, 0).UTC()
	// Walk the root chain: each next version must be signed by the current
	// root and by itself.
	for {
		next := filepath.Join(t.Dir, tufRootsDir, fmt.Sprintf("%d.root.json", tm.Root.Signed.Version+1))
		rootData, err := os.ReadFile(next)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, err
		}
		if _, err := tm.UpdateRoot(rootData); err != nil {
			return nil, fmt.Errorf("cached TUF root %s: %w", filepath.Base(next), err)
		}
	}
	metaDir := filepath.Join(t.Dir, "tuf", tuf.URLToPath(t.repositoryURL()))
	read := func(role string) ([]byte, error) { return os.ReadFile(filepath.Join(metaDir, role+".json")) }
	ts, err := read(metadata.TIMESTAMP)
	if err != nil {
		return nil, err
	}
	if _, err := tm.UpdateTimestamp(ts); err != nil {
		return nil, fmt.Errorf("cached TUF timestamp: %w", err)
	}
	snap, err := read(metadata.SNAPSHOT)
	if err != nil {
		return nil, err
	}
	if _, err := tm.UpdateSnapshot(snap, false); err != nil {
		return nil, fmt.Errorf("cached TUF snapshot: %w", err)
	}
	tgt, err := read(metadata.TARGETS)
	if err != nil {
		return nil, err
	}
	targets, err := tm.UpdateDelegatedTargets(tgt, metadata.TARGETS, metadata.ROOT)
	if err != nil {
		return nil, fmt.Errorf("cached TUF targets: %w", err)
	}
	info, ok := targets.Signed.Targets[trustedRootFile]
	if !ok {
		return nil, fmt.Errorf("the signed TUF targets do not list %s", trustedRootFile)
	}
	if err := info.VerifyLengthHashes(data); err != nil {
		return nil, fmt.Errorf("the cached %s does not match the signed TUF targets: %w", trustedRootFile, err)
	}
	return data, nil
}

// rootRecorder remembers the root metadata versions a TUF fetch downloads.
type rootRecorder struct {
	fetcher.Fetcher
	mu    sync.Mutex
	roots map[string][]byte
}

var rootVersionFile = regexp.MustCompile(`^[0-9]+\.root\.json$`)

func (r *rootRecorder) DownloadFile(urlPath string, maxLength int64, timeout time.Duration) ([]byte, error) {
	data, err := r.Fetcher.DownloadFile(urlPath, maxLength, timeout)
	if err == nil {
		name := path.Base(urlPath)
		if i := strings.IndexAny(name, "?#"); i >= 0 {
			name = name[:i]
		}
		if rootVersionFile.MatchString(name) {
			r.mu.Lock()
			if r.roots == nil {
				r.roots = map[string][]byte{}
			}
			r.roots[name] = data
			r.mu.Unlock()
		}
	}
	return data, err
}

// save writes the recorded root versions; failures only cost the offline
// fallback.
func (r *rootRecorder) save(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.roots) == 0 {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	for name, data := range r.roots {
		_ = writeFileAtomic(filepath.Join(dir, name), data, 0o600)
	}
}
