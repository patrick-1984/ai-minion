package web

// The hosted installer of a Hub's own release (deploy/install.sh --join). The
// Add worker page shows one command that downloads install.sh of the release
// this Hub runs to a private file, checks its SHA-256 against the digest in the
// signed release manifest of that exact tag, and only then runs it. The Hub
// learns the digest from the signed manifest (version.Updater.VerifiedInstaller,
// wired in by the server); a development build, or a release that cannot be
// verified, has none and the page shows the manual commands instead.

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// releaseDownloadBase is where the assets of a release are served.
const releaseDownloadBase = "https://github.com/patrick-1984/ai-minion/releases/download/"

// InstallerOffer is the verified installer of this Hub's release.
type InstallerOffer struct {
	// Tag is the release (vX.Y.Z), URL the installer of that release and
	// SHA256 its lower-case hex digest from the signed manifest.
	Tag, URL, SHA256 string
}

var installerSource atomic.Pointer[func(ctx context.Context, tag string) (string, error)]

// SetInstallerSource sets how the Hub learns the verified SHA-256 of the
// install script of an exact release tag. nil removes it. The returned function
// removes this source again while it is still the current one.
func SetInstallerSource(f func(ctx context.Context, tag string) (string, error)) (release func()) {
	installerMu.Lock()
	installerCache = map[string]installerEntry{}
	installerMu.Unlock()
	if f == nil {
		installerSource.Store(nil)
		return func() {}
	}
	p := &f
	installerSource.Store(p)
	return func() { installerSource.CompareAndSwap(p, nil) }
}

type installerEntry struct {
	sha     string
	failed  bool
	at      time.Time
	loading bool
}

var (
	installerMu    sync.Mutex
	installerCache = map[string]installerEntry{}
)

// installerRetry is how long a failed lookup is not repeated.
const installerRetry = time.Minute

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// installerTag returns the release tag of this Hub, or "" for a development
// build (which has no release to verify).
func installerTag() string {
	v, err := version.ParseVersion(hubVersion())
	if err != nil {
		return ""
	}
	return v.String()
}

// installerOffer returns the verified installer of this Hub's release. It
// waits for the lookup at most until ctx ends; a result is cached, a failure
// for a minute.
func installerOffer(ctx context.Context) (InstallerOffer, bool) {
	tag := installerTag()
	src := installerSource.Load()
	if tag == "" || src == nil {
		return InstallerOffer{}, false
	}
	installerMu.Lock()
	e := installerCache[tag]
	if e.sha != "" {
		installerMu.Unlock()
		return offerFor(tag, e.sha), true
	}
	if e.failed && time.Since(e.at) < installerRetry {
		installerMu.Unlock()
		return InstallerOffer{}, false
	}
	installerMu.Unlock()
	sha, err := (*src)(ctx, tag)
	installerMu.Lock()
	defer installerMu.Unlock()
	if err != nil || !sha256Hex.MatchString(sha) {
		installerCache[tag] = installerEntry{failed: true, at: time.Now()}
		return InstallerOffer{}, false
	}
	installerCache[tag] = installerEntry{sha: sha}
	return offerFor(tag, sha), true
}

func offerFor(tag, sha string) InstallerOffer {
	return InstallerOffer{Tag: tag, URL: releaseDownloadBase + tag + "/install.sh", SHA256: sha}
}

// warmInstaller starts the lookup in the background, so the first command
// the page creates does not wait for it.
func warmInstaller() {
	tag := installerTag()
	if tag == "" || installerSource.Load() == nil {
		return
	}
	installerMu.Lock()
	e := installerCache[tag]
	if e.sha != "" || e.loading || (e.failed && time.Since(e.at) < installerRetry) {
		installerMu.Unlock()
		return
	}
	e.loading = true
	installerCache[tag] = e
	installerMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		installerMu.Lock()
		e := installerCache[tag]
		e.loading = false
		installerCache[tag] = e
		installerMu.Unlock()
		installerOffer(ctx)
	}()
}

// installLine is the one-line command that downloads the installer of the
// release to a private file, requires it to be non-empty, checks its SHA-256
// against the signed digest and runs it with --join. With token the join token
// is in the environment of the installer only (never in an argument list);
// without it the installer asks for the token at a masked prompt on the
// terminal. The file is removed afterwards, whatever happened.
func installLine(o InstallerOffer, token string) string {
	var b strings.Builder
	b.WriteString(`f=$(mktemp "${TMPDIR:-/tmp}/minion-install.XXXXXX") && curl -fsSL ` + shellArg(o.URL) + ` -o "$f" && [ -s "$f" ] && `)
	b.WriteString(`{ sha256sum "$f" 2>/dev/null || shasum -a 256 "$f"; } | grep -q '^` + o.SHA256 + ` ' && `)
	b.WriteString("MINION_VERSION=" + o.Tag + " ")
	if token != "" {
		b.WriteString("MINION_JOIN=" + shellArg(token) + " ")
	}
	b.WriteString(`sh "$f" --join; rm -f "$f"`)
	return b.String()
}
