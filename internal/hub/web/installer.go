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

// installerJoinSince is the first release whose install.sh has --join. A
// Hub that runs an older version, or a development build, does not offer the
// one-line commands: its release's installer cannot join.
const installerJoinSince = "v0.17.0"

// InstallerOffer is the installer of this Hub's release.
type InstallerOffer struct {
	// Tag is the release (vX.Y.Z), URL the installer of that release,
	// SHA256 its lower-case hex digest and ChecksumsSHA256 the digest of the
	// release's checksums.txt, both from the signed manifest and checksums.
	Tag, URL, SHA256, ChecksumsSHA256 string
}

var installerSource atomic.Pointer[func(ctx context.Context, tag string) (version.InstallerInfo, error)]

// SetInstallerSource sets how the Hub learns the verified digests of the
// install script of an exact release tag. The function must not contact
// anything while the Hub's update check is off. nil removes it. The returned
// function removes this source again while it is still the current one.
func SetInstallerSource(f func(ctx context.Context, tag string) (version.InstallerInfo, error)) (release func()) {
	installerMu.Lock()
	installerState = installerEntry{}
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
	info     version.InstallerInfo
	ok       bool
	failedAt time.Time
	inflight bool
}

var (
	installerMu    sync.Mutex
	installerState installerEntry
)

// installerRetry is how long a failed lookup is not repeated.
const installerRetry = time.Minute

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// installerTag returns the release tag of this Hub, or "" when the Hub has no
// release whose installer can join (a development build, or a release older
// than installerJoinSince).
func installerTag() string {
	v, err := version.ParseVersion(hubVersion())
	if err != nil {
		return ""
	}
	min, _ := version.ParseVersion(installerJoinSince)
	core := v
	core.Pre = ""
	if core.Compare(min) < 0 {
		return ""
	}
	return v.String()
}

// usageInstallerArgs only selects a flag understood by the packaged Worker
// and installer. The enrollment handshake decides which policy applies.
func usageInstallerArgs(tag, flag string) string {
	const since = "v0.18.0"
	v, err := version.ParseVersion(tag)
	if err != nil {
		return ""
	}
	v.Pre = ""
	min, _ := version.ParseVersion(since)
	if v.Compare(min) < 0 {
		return ""
	}
	return " " + flag
}

// cachedInstaller returns the verified installer of this Hub's release if the
// warmer has found it. It never waits and never does I/O.
func cachedInstaller() (InstallerOffer, bool) {
	tag := installerTag()
	if tag == "" || installerSource.Load() == nil {
		return InstallerOffer{}, false
	}
	installerMu.Lock()
	defer installerMu.Unlock()
	if !installerState.ok {
		return InstallerOffer{}, false
	}
	return InstallerOffer{Tag: tag, URL: releaseDownloadBase + tag + "/install.sh",
		SHA256: installerState.info.InstallerSHA256, ChecksumsSHA256: installerState.info.ChecksumsSHA256}, true
}

// warmInstaller starts the lookup in the background, once at a time (a lookup
// in flight is never started again, and one that failed waits a minute). Its
// context is its own, so a canceled request cannot spoil it, and a good
// cached result is never replaced by a later failure.
func warmInstaller() {
	tag := installerTag()
	src := installerSource.Load()
	if tag == "" || src == nil {
		return
	}
	installerMu.Lock()
	e := installerState
	if e.ok || e.inflight || (!e.failedAt.IsZero() && time.Since(e.failedAt) < installerRetry) {
		installerMu.Unlock()
		return
	}
	installerState.inflight = true
	installerMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		info, err := (*src)(ctx, tag)
		installerMu.Lock()
		defer installerMu.Unlock()
		// A source that was replaced meanwhile owns the state now.
		if installerSource.Load() != src {
			return
		}
		installerState.inflight = false
		if installerState.ok {
			return
		}
		if err != nil || !sha256Hex.MatchString(info.InstallerSHA256) || !sha256Hex.MatchString(info.ChecksumsSHA256) {
			installerState.failedAt = time.Now()
			return
		}
		installerState.info, installerState.ok = info, true
	}()
}

// nameArg is " --name <name>" for a named Worker, else "".
func nameArg(name string) string {
	if name == "" {
		return ""
	}
	return " --name " + shellArg(name)
}

// installLine is the one-line command that downloads the installer of the
// release to a private file, requires it to be non-empty, checks its SHA-256
// against the signed digest and runs it with --join. The installer is also
// told the SHA-256 of the signed checksums.txt, so it authenticates the
// archive it installs, not only itself. With token the join token is in the
// environment of the installer only (never in an argument list); without it
// the variable is set empty (an exported old value is not used) and the
// installer asks at a masked prompt on the terminal. The file is removed
// afterwards and the status of the installer is the status of the command.
func installLine(o InstallerOffer, token, name string) string {
	var b strings.Builder
	b.WriteString(`(f=$(mktemp "${TMPDIR:-/tmp}/minion-install.XXXXXX") && curl -fsSL ` + shellArg(o.URL) + ` -o "$f" && [ -s "$f" ] && `)
	b.WriteString(`{ sha256sum "$f" 2>/dev/null || shasum -a 256 "$f"; } | grep -q '^` + o.SHA256 + ` ' && `)
	b.WriteString("MINION_VERSION=" + o.Tag + " MINION_CHECKSUMS_SHA256=" + o.ChecksumsSHA256 + " " + joinEnv(token))
	b.WriteString(`sh "$f" --join` + nameArg(name) + usageInstallerArgs(o.Tag, "--accept-usage") + `; s=$?; rm -f "$f"; exit $s)`)
	return b.String()
}

// joinEnv is the MINION_JOIN assignment of a command: the token, or empty so
// that the installer prompts whatever the shell has exported.
func joinEnv(token string) string {
	if token == "" {
		return "MINION_JOIN= "
	}
	return "MINION_JOIN=" + shellArg(token) + " "
}

// pipeLine is the simple command: the versioned installer of this Hub's
// release over HTTPS, piped into sh. install.sh runs from its last line, so a
// cut-short download runs nothing, and it verifies the binary against the
// release checksums.
func pipeLine(tag, token, name string) string {
	return installerModeLine(tag, token, name, "--join")
}

func installerModeLine(tag, token, name, mode string) string {
	return downloadedInstallerLine(tag, "install.sh", "MINION_VERSION="+tag+" "+joinEnv(token)+"sh -s -- "+mode+nameArg(name)+usageInstallerArgs(tag, "--accept-usage"))
}

func windowsInstallerLine(tag, name string) string {
	line := `[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12; & ([scriptblock]::Create((irm '` + releaseDownloadBase + tag + `/install.ps1'))) -Join -Version ` + tag
	if name != "" {
		line += " -Name " + psArg(name)
	}
	return line + usageInstallerArgs(tag, "-AcceptUsage")
}

func androidInstallerLine(tag string) string {
	return deviceInstallerLine(tag, "android.sh", "--version "+tag)
}

func deviceInstallerLine(tag, asset, args string) string {
	return downloadedInstallerLine(tag, asset, "sh -s -- "+args)
}

// The suffix detects an empty or incomplete script even when curl succeeds.
// The token is assigned only to the shell that executes the complete script.
func downloadedInstallerLine(tag, asset, run string) string {
	return `(s=$(curl -fsSL ` + shellArg(releaseDownloadBase+tag+"/"+asset) + `) && case "$s" in *'# minion installer complete') printf '%s\n' "$s" | ` + run + `;; *) echo "Incomplete Minion installer" >&2; exit 1;; esac)`
}
