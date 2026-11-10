package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
)

// This file is the only place in the package that creates an HTTP client
// (AD-19: the binary makes outbound connections only during an explicit
// "minion update"). TestNoHTTPOutsideUpdate keeps it that way. The clients are
// built by [NewGitHubSource] and [newTUFFetcher], which the update command
// reaches; nothing in this package builds one at init time or lazily on its
// own.
//
// An online update has two outbound destinations: the release host
// (api.github.com and the release asset hosts it redirects to) and, for the
// Sigstore trusted root, the public TUF repository (tuf-repo-cdn.sigstore.dev,
// through sigstore-go, see [TUFTrustedRoot]). The second one is not the
// release host that AD-19 and NFR8 name. "minion update --offline" contacts
// nothing.

// Errors a caller can test for with errors.Is.
var (
	// ErrNetwork means the release host (or the Sigstore trust root) could
	// not be reached.
	ErrNetwork = errors.New("cannot reach the release host")
	// ErrDowngrade means the resolved version is older than the running one.
	ErrDowngrade = errors.New("refusing to downgrade")
	// ErrChecksum means a download does not match its signed checksum.
	ErrChecksum = errors.New("checksum mismatch")
	// ErrNoCached means no verified cached version matches the selector.
	ErrNoCached = errors.New("no verified cached version matches")
)

// Source is the port to the release host: it lists releases and opens their
// assets. [GitHubSource] is the production adapter; tests use a fake.
type Source interface {
	// Releases lists the published releases. A transport failure is
	// reported as an error that wraps [ErrNetwork].
	Releases(ctx context.Context) ([]Release, error)
	// Open streams one asset. The caller closes the reader.
	Open(ctx context.Context, a Asset) (io.ReadCloser, error)
}

// VerifierFactory builds the signature verifier for one run. offline asks
// for a verifier that needs no network.
type VerifierFactory func(ctx context.Context, offline bool) (BlobVerifier, error)

// SigstoreVerifierFactory returns a [VerifierFactory] that verifies with
// sigstore-go against the material of p.
func SigstoreVerifierFactory(p TrustedRootProvider) VerifierFactory {
	return func(ctx context.Context, offline bool) (BlobVerifier, error) {
		m, err := p.TrustedMaterial(ctx, offline)
		if err != nil {
			return nil, err
		}
		return NewSigstoreVerifier(m), nil
	}
}

// Options are the inputs of one update run.
type Options struct {
	// Selector picks the release.
	Selector Selector
	// Current is the running version ("dev" for an unversioned build). It
	// is compared with the resolved version.
	Current string
	// AllowDowngrade permits installing a version older than Current.
	AllowDowngrade bool
	// Offline installs the newest verified cached version that matches the
	// selector and never touches the network.
	Offline bool
	// HubRange is the protocol range the connected Hub supports (from its
	// welcome frame). Nil means no Hub connection: nothing to check.
	HubRange *ProtoRange
	// ExecPath is the binary to replace; it defaults to the running one.
	ExecPath string
	// GOOS and GOARCH select the archive; they default to the running
	// platform.
	GOOS, GOARCH string
}

func (o Options) withDefaults() (Options, error) {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.ExecPath == "" {
		p, err := os.Executable()
		if err != nil {
			return o, fmt.Errorf("cannot locate the running binary: %w", err)
		}
		o.ExecPath = p
	}
	return o, nil
}

// Result describes what a run did.
type Result struct {
	// Tag and Version are the resolved release.
	Tag     string
	Version SemVer
	// Previous is the version that was running.
	Previous string
	// UpToDate is true when the resolved version is the running one and
	// nothing was replaced.
	UpToDate bool
	// FromCache is true when the version came from the offline cache.
	FromCache bool
	// Path is the replaced binary.
	Path string
}

// Updater downloads, verifies, caches and installs a release.
type Updater struct {
	// Source is required unless every run is offline.
	Source Source
	// Verifier builds the signature verifier (see [SigstoreVerifierFactory]).
	Verifier VerifierFactory
	// Cache keeps verified releases for offline use.
	Cache Cache
	// Out receives one-line progress messages; nil discards them.
	Out io.Writer
}

func (u *Updater) logf(format string, args ...any) {
	if u.Out != nil {
		_, _ = fmt.Fprintf(u.Out, format+"\n", args...)
	}
}

// Run resolves the selector, verifies the release and replaces the binary.
func (u *Updater) Run(ctx context.Context, o Options) (Result, error) {
	o, err := o.withDefaults()
	if err != nil {
		return Result{}, err
	}
	if u.Verifier == nil {
		return Result{}, errors.New("update: no signature verifier configured")
	}
	if o.Offline {
		return u.runOffline(ctx, o)
	}
	if u.Source == nil {
		return Result{}, errors.New("update: no release source configured")
	}

	releases, err := u.Source.Releases(ctx)
	if err != nil {
		return Result{}, withOfflineHint(err)
	}
	rel, v, err := o.Selector.Resolve(releases)
	if err != nil {
		return Result{}, err
	}
	res := Result{Tag: rel.Tag, Version: v, Previous: o.Current, Path: o.ExecPath}
	if upToDate, err := checkVersion(o, v); err != nil {
		return res, err
	} else if upToDate {
		res.UpToDate = true
		u.logf("minion %s is already installed", rel.Tag)
		return res, nil
	}
	u.logf("resolved %s to %s", o.Selector, rel.Tag)

	// The cache holds the Sigstore trust root and the verified release. Before
	// anything is written to it, or read from it, it must be a directory that
	// only trusted users can change.
	if err := u.Cache.Prepare(); err != nil {
		return res, err
	}
	verifier, err := u.Verifier(ctx, false)
	if err != nil {
		return res, withOfflineHint(err)
	}
	meta, err := u.fetchMetadata(ctx, verifier, rel)
	if err != nil {
		return res, err
	}
	if err := checkProtocol(meta.manifest, o.HubRange); err != nil {
		return res, err
	}

	archive := ArchiveName(rel.Tag, o.GOOS, o.GOARCH, goarmFor(o.GOARCH))
	want, ok := meta.checksums[archive]
	if !ok {
		return res, fmt.Errorf("release %s has no archive for %s/%s (%s)", rel.Tag, o.GOOS, o.GOARCH, archive)
	}
	if md, listed := meta.manifest.Digest(archive); listed && md != want {
		return res, fmt.Errorf("%w: release manifest and checksums.txt disagree about %s", ErrChecksum, archive)
	}
	asset, ok := rel.Asset(archive)
	if !ok {
		return res, fmt.Errorf("release %s has no asset %s", rel.Tag, archive)
	}

	staging, err := u.Cache.NewStaging()
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(staging) }() // a no-op once committed

	u.logf("downloading %s", archive)
	// The archive is verified against the signed digest while it is written,
	// and the executable is extracted from that same open file, never from the
	// path again.
	af, err := u.download(ctx, asset, filepath.Join(staging, archive), want)
	if err != nil {
		return res, err
	}
	binSum, err := extractBinary(af, archive, o.GOOS, filepath.Join(staging, BinaryName(o.GOOS)))
	if cerr := af.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return res, err
	}
	// The archive stays in the cache entry: it is the one file whose digest
	// the signed checksums vouch for, so an offline install can authenticate
	// the executable from it (see verifyCached).
	if err := meta.writeTo(staging); err != nil {
		return res, err
	}
	if err := writeMeta(staging, CacheMeta{
		Tag: rel.Tag, GOOS: o.GOOS, GOARCH: o.GOARCH,
		ArchiveName: archive, ArchiveSHA256: want, BinarySHA256: binSum,
		Prerelease: rel.Prerelease,
	}); err != nil {
		return res, err
	}
	if err := u.Cache.Commit(staging, rel.Tag); err != nil {
		return res, err
	}

	u.logf("installing %s", rel.Tag)
	// binSum is the digest of the bytes extracted from the verified archive.
	// The copy that is installed is hashed again and must equal it.
	if err := ReplaceBinary(o.ExecPath, filepath.Join(u.Cache.EntryDir(rel.Tag), BinaryName(o.GOOS)), o.GOOS, binSum); err != nil {
		return res, err
	}
	return res, nil
}

// runOffline installs the newest verified cached version that matches the
// selector. The signatures are verified again, with the cached trusted root,
// and the executable is taken from the cached archive, whose digest the
// signed checksums vouch for.
func (u *Updater) runOffline(ctx context.Context, o Options) (Result, error) {
	// A cache that exists must be trusted before anything in it is read,
	// including the cached trust root. A missing one is simply empty.
	if _, err := u.Cache.prepareExisting(); err != nil {
		return Result{}, err
	}
	tags, err := u.Cache.Tags()
	if err != nil {
		return Result{}, err
	}
	verifier, err := u.Verifier(ctx, true)
	if err != nil {
		return Result{}, err
	}
	var (
		problems []string
		staging  string // scratch space for the authenticated executable
	)
	defer func() {
		if staging != "" {
			_ = os.RemoveAll(staging)
		}
	}()
	for _, v := range tags { // newest first
		if !o.Selector.Matches(v) {
			continue
		}
		tag := v.String()
		meta, err := readMeta(u.Cache.EntryDir(tag))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", tag, err))
			continue
		}
		// A release the host flagged as a pre-release never satisfies a
		// channel, online or offline, whatever its tag looks like.
		if meta.Prerelease && o.Selector.IsChannel() {
			continue
		}
		if staging == "" {
			if staging, err = u.Cache.NewStaging(); err != nil {
				return Result{}, err
			}
		}
		bin := filepath.Join(staging, BinaryName(o.GOOS))
		_ = os.Remove(bin)
		manifest, binSum, err := u.verifyCached(verifier, tag, meta, o, bin)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", tag, err))
			continue
		}
		res := Result{Tag: tag, Version: v, Previous: o.Current, FromCache: true, Path: o.ExecPath}
		if upToDate, err := checkVersion(o, v); err != nil {
			return res, err
		} else if upToDate {
			res.UpToDate = true
			u.logf("minion %s is already installed", tag)
			return res, nil
		}
		if err := checkProtocol(manifest, o.HubRange); err != nil {
			return res, err
		}
		u.logf("installing cached %s", tag)
		if err := ReplaceBinary(o.ExecPath, bin, o.GOOS, binSum); err != nil {
			return res, err
		}
		return res, nil
	}
	err = fmt.Errorf("%w %q; run minion update once while online to fill the cache", ErrNoCached, o.Selector.String())
	if len(problems) > 0 {
		err = fmt.Errorf("%w (unusable cache entries: %s)", err, strings.Join(problems, "; "))
	}
	return Result{}, err
}

// checkVersion applies the downgrade rule. It reports upToDate when the
// resolved version is the running one. An unversioned ("dev") build has
// nothing to compare with and always proceeds.
func checkVersion(o Options, resolved SemVer) (upToDate bool, err error) {
	cur, perr := ParseVersion(o.Current)
	if perr != nil {
		return false, nil
	}
	switch c := resolved.Compare(cur); {
	case c == 0:
		return true, nil
	case c < 0 && !o.AllowDowngrade:
		return false, fmt.Errorf("%w from %s to %s; pass --allow-downgrade to install the older version", ErrDowngrade, cur, resolved)
	}
	return false, nil
}

// checkProtocol refuses a release whose protocol range does not overlap the
// connected Hub's (A-10). Without a Hub connection there is nothing to check.
func checkProtocol(m Manifest, hub *ProtoRange) error {
	if hub == nil {
		return nil
	}
	next, err := m.Range()
	if err != nil {
		return err
	}
	_, err = NegotiateRanges(next, *hub)
	return err
}

func goarmFor(goarch string) string {
	if goarch == "arm" {
		return "7" // the only 32-bit ARM target of the release (AD-24)
	}
	return ""
}

func withOfflineHint(err error) error {
	if errors.Is(err, ErrNetwork) {
		return fmt.Errorf("%w; no network? use --offline to install the newest verified cached version", err)
	}
	return err
}

// verifiedMeta is the signed metadata of one release.
type verifiedMeta struct {
	checksumsRaw, checksumsBundle []byte
	manifestRaw, manifestBundle   []byte
	checksums                     Checksums
	manifest                      Manifest
}

// writeTo stores the signed metadata next to a cached binary, so the entry
// can be verified again offline.
func (m verifiedMeta) writeTo(dir string) error {
	for name, data := range map[string][]byte{
		ChecksumsAsset:             m.checksumsRaw,
		BundleName(ChecksumsAsset): m.checksumsBundle,
		ManifestAsset:              m.manifestRaw,
		BundleName(ManifestAsset):  m.manifestBundle,
	} {
		if err := writeFileAtomic(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// fetchMetadata downloads checksums.txt and release-manifest.json with their
// bundles and verifies them.
func (u *Updater) fetchMetadata(ctx context.Context, verifier BlobVerifier, rel Release) (verifiedMeta, error) {
	var m verifiedMeta
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{
		{ChecksumsAsset, &m.checksumsRaw},
		{BundleName(ChecksumsAsset), &m.checksumsBundle},
		{ManifestAsset, &m.manifestRaw},
		{BundleName(ManifestAsset), &m.manifestBundle},
	} {
		a, ok := rel.Asset(f.name)
		if !ok {
			return m, fmt.Errorf("release %s has no %s; it cannot be verified", rel.Tag, f.name)
		}
		data, err := u.readAsset(ctx, a, maxManifestSize)
		if err != nil {
			return m, err
		}
		*f.dst = data
	}
	return m, m.verify(verifier, rel.Tag)
}

// verify checks both signatures and parses the signed files. The manifest's
// artifact digests must agree with the signed checksums.txt.
func (m *verifiedMeta) verify(verifier BlobVerifier, tag string) error {
	if err := verifier.VerifyBlob(m.checksumsRaw, m.checksumsBundle, tag); err != nil {
		return fmt.Errorf("%s: %w", ChecksumsAsset, err)
	}
	if err := verifier.VerifyBlob(m.manifestRaw, m.manifestBundle, tag); err != nil {
		return fmt.Errorf("%s: %w", ManifestAsset, err)
	}
	var err error
	if m.checksums, err = ParseChecksums(m.checksumsRaw); err != nil {
		return err
	}
	if m.manifest, err = ParseManifest(m.manifestRaw, tag); err != nil {
		return err
	}
	for _, a := range m.manifest.Artifacts {
		if sum, ok := m.checksums[a.Name]; ok && sum != a.SHA256 {
			return fmt.Errorf("%w: release manifest and checksums.txt disagree about %s", ErrChecksum, a.Name)
		}
	}
	return nil
}

// verifyCached re-verifies a cache entry and writes the executable it
// authenticated to dst, returning the signed manifest and the SHA-256 of that
// executable. Everything that is trusted comes from the signed files: both
// signatures are verified, the cached archive must have the digest the signed
// checksums.txt lists for this platform's archive, and the executable is
// extracted from that archive. meta.json is not signed, so it is never believed
// on its own; the cached binary next to it must equal the extracted one, which
// keeps a damaged entry out.
//
// The cache files are opened once each, without following links, and checked on
// the handle (see [openTrusted]). The archive is hashed and then extracted
// through the same open handle, so the executable comes from the bytes whose
// digest matched, even if the path is replaced in between.
func (u *Updater) verifyCached(verifier BlobVerifier, tag string, meta CacheMeta, o Options, dst string) (Manifest, string, error) {
	dir := u.Cache.EntryDir(tag)
	archive := ArchiveName(tag, o.GOOS, o.GOARCH, goarmFor(o.GOARCH))
	if meta.Tag != tag || meta.GOOS != o.GOOS || meta.GOARCH != o.GOARCH || meta.ArchiveName != archive {
		return Manifest{}, "", errors.New("cached for a different platform")
	}
	var m verifiedMeta
	var err error
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{
		{ChecksumsAsset, &m.checksumsRaw},
		{BundleName(ChecksumsAsset), &m.checksumsBundle},
		{ManifestAsset, &m.manifestRaw},
		{BundleName(ManifestAsset), &m.manifestBundle},
	} {
		if *f.dst, err = readTrustedFile(filepath.Join(dir, f.name), maxManifestSize); err != nil {
			return Manifest{}, "", err
		}
	}
	if err := m.verify(verifier, tag); err != nil {
		return Manifest{}, "", err
	}
	want, ok := m.checksums[archive]
	if !ok {
		return Manifest{}, "", fmt.Errorf("the signed checksums list no %s", archive)
	}
	af, err := openTrusted(filepath.Join(dir, archive))
	if err != nil {
		return Manifest{}, "", err
	}
	defer func() { _ = af.Close() }()
	got, err := handleSHA256(af)
	if err != nil {
		return Manifest{}, "", err
	}
	if got != want {
		return Manifest{}, "", fmt.Errorf("%w: the cached archive differs from the signed checksums", ErrChecksum)
	}
	sum, err := extractBinary(af, archive, o.GOOS, dst)
	if err != nil {
		return Manifest{}, "", err
	}
	bf, err := openTrusted(filepath.Join(dir, BinaryName(o.GOOS)))
	if err != nil {
		_ = os.Remove(dst)
		return Manifest{}, "", err
	}
	defer func() { _ = bf.Close() }()
	cached, err := handleSHA256(bf)
	if err != nil {
		_ = os.Remove(dst)
		return Manifest{}, "", err
	}
	if cached != sum || meta.BinarySHA256 != sum {
		_ = os.Remove(dst)
		return Manifest{}, "", fmt.Errorf("%w: the cached binary was modified", ErrChecksum)
	}
	return m.manifest, sum, nil
}

// handleSHA256 returns the SHA-256 of the whole content of an open file,
// whatever its read offset is.
func handleSHA256(f *os.File) (string, error) {
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, fi.Size())); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// maxArchiveSize bounds an archive download.
const maxArchiveSize = 512 << 20

func (u *Updater) readAsset(ctx context.Context, a Asset, limit int64) ([]byte, error) {
	rc, err := u.Source.Open(ctx, a)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrNetwork, a.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", a.Name, limit)
	}
	return data, nil
}

// download streams an asset to dst and checks its SHA-256 against the signed
// digest. It returns the file, still open for reading, whose bytes matched: the
// caller extracts from this handle and closes it, and never reopens dst by path
// (see extractBinary). The file is removed on any failure.
func (u *Updater) download(ctx context.Context, a Asset, dst, wantSHA256 string) (f *os.File, err error) {
	rc, err := u.Source.Open(ctx, a)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	f, err = os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(dst)
			f = nil
		}
	}()
	if err = checkNewFile(f); err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(rc, maxArchiveSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: downloading %s: %w", ErrNetwork, a.Name, err)
	}
	if n > maxArchiveSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", a.Name, int64(maxArchiveSize))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return nil, fmt.Errorf("%w: %s has sha256 %s, the signed checksums say %s", ErrChecksum, a.Name, got, wantSHA256)
	}
	return f, nil
}

// ---- GitHub adapter -------------------------------------------------------

// DefaultAPIURL is the GitHub REST API root.
const DefaultAPIURL = "https://api.github.com"

// GitHubSource is the [Source] over the GitHub releases API of this
// repository.
type GitHubSource struct {
	apiURL    string
	userAgent string
	client    *http.Client
}

// NewGitHubSource returns a source that lists releases at apiURL (use
// [DefaultAPIURL]) for this repository. It creates the process's only HTTP
// client of this package. apiURL must be https, or http on a loopback host
// (a test or a local mirror).
func NewGitHubSource(apiURL, userAgent string) (*GitHubSource, error) {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	if err := checkURL(apiURL); err != nil {
		return nil, fmt.Errorf("release API URL: %w", err)
	}
	return &GitHubSource{
		apiURL:    strings.TrimRight(apiURL, "/"),
		userAgent: userAgent,
		client:    newHTTPClient(),
	}, nil
}

func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return checkURL(req.URL.String())
		},
	}
}

// checkURL accepts https URLs and http URLs on a loopback host only.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a valid URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("%q must use https", u.Redacted())
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

const (
	ghPageSize = 100
	ghMaxPages = 10
	ghMaxBody  = 16 << 20
)

// Releases implements [Source].
func (g *GitHubSource) Releases(ctx context.Context) ([]Release, error) {
	var out []Release
	for page := 1; page <= ghMaxPages; page++ {
		u := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d&page=%d", g.apiURL, RepoOwner, RepoName, ghPageSize, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		g.setUserAgent(req)
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNetwork, err)
		}
		var list []ghRelease
		err = decodeReleases(resp, &list)
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			rel := Release{Tag: r.TagName, Draft: r.Draft, Prerelease: r.Prerelease}
			for _, a := range r.Assets {
				rel.Assets = append(rel.Assets, Asset(a))
			}
			out = append(out, rel)
		}
		if len(list) < ghPageSize {
			break
		}
	}
	return out, nil
}

func decodeReleases(resp *http.Response, list *[]ghRelease) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("listing releases: HTTP %d from the release host", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, ghMaxBody)).Decode(list); err != nil {
		return fmt.Errorf("listing releases: %w", err)
	}
	return nil
}

// Open implements [Source].
func (g *GitHubSource) Open(ctx context.Context, a Asset) (io.ReadCloser, error) {
	if err := checkURL(a.URL); err != nil {
		return nil, fmt.Errorf("asset %s: %w", a.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	g.setUserAgent(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNetwork, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("downloading %s: HTTP %d from the release host", a.Name, resp.StatusCode)
	}
	return resp.Body, nil
}

func (g *GitHubSource) setUserAgent(req *http.Request) {
	ua := g.userAgent
	if ua == "" {
		ua = "minion-update"
	}
	req.Header.Set("User-Agent", ua)
}

// ---- Sigstore TUF fetcher --------------------------------------------------

// tufRequestTimeout bounds one request to the TUF repository, response body
// included.
const tufRequestTimeout = 2 * time.Minute

// ctxDoer sends every request of the TUF client with ctx, because go-tuf
// builds its requests without one.
type ctxDoer struct {
	ctx    context.Context
	client *http.Client
}

func (d ctxDoer) Do(req *http.Request) (*http.Response, error) {
	return d.client.Do(req.WithContext(d.ctx))
}

// newTUFFetcher returns the fetcher TUFTrustedRoot hands to sigstore-go. It
// uses the hardened client of this file (https or loopback only, bounded
// redirects, connection and header timeouts), a bound on every request, and
// ctx, so that cancelling the update stops a stalled trust-root download.
func newTUFFetcher(ctx context.Context) fetcher.Fetcher {
	client := newHTTPClient()
	client.Timeout = tufRequestTimeout
	f := fetcher.NewDefaultFetcher()
	f.SetHTTPUserAgent("minion-update")
	f.SetHTTPClient(ctxDoer{ctx: ctx, client: client})
	return f
}
