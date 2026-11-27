package version

// The Android app's self-update (design WP2/WP3): the Worker inside the app
// downloads the signed APK of one exact release, verifies it with the same
// Sigstore chain as a binary update and stages it in the app's private
// storage; the app installs it with PackageInstaller. Two kinds of release
// carry an APK:
//
//   - a full release "vX.Y.Z[-rc.N]" (release.yml): the APK is listed in the
//     signed checksums.txt, and the signed release-manifest.json gives the
//     protocol range;
//   - an APK-only release "android-vX.Y.Z" (android-release.yml): exactly the
//     APK, checksums.txt and its bundle, signed by android-release.yml for that
//     tag. It has no manifest, so its protocol range is that of its base: the
//     newest full release vX.Y.* at or below X.Y.Z (android-release.yml is
//     tagged above the newest full release and below the next one, from the
//     same line). Without a base the APK is refused.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// AndroidTagPrefix starts the tag of an APK-only release, "android-vX.Y.Z".
const AndroidTagPrefix = "android-"

// ErrUpToDate means the release to stage is the version already installed.
var ErrUpToDate = errors.New("already installed")

var androidTagRE = regexp.MustCompile(`^android-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// ParseReleaseTag parses a tag the verifier accepts: a full release
// "vX.Y.Z[-rc.N]" or an APK-only release "android-vX.Y.Z" (no pre-release,
// as android-release.yml allows). apkOnly reports the second kind. The tag
// must be in its canonical form.
func ParseReleaseTag(tag string) (v SemVer, apkOnly bool, err error) {
	if strings.HasPrefix(tag, AndroidTagPrefix) {
		if !androidTagRE.MatchString(tag) {
			return SemVer{}, false, fmt.Errorf("%q is not an APK-only release tag android-vX.Y.Z", tag)
		}
		v, err = ParseVersion(strings.TrimPrefix(tag, AndroidTagPrefix))
		return v, true, err
	}
	v, err = ParseVersion(tag)
	if err != nil {
		return SemVer{}, false, err
	}
	if v.String() != tag {
		return SemVer{}, false, fmt.Errorf("%q is not a release tag vX.Y.Z", tag)
	}
	return v, false, nil
}

// APKAssetName is the name of the signed universal APK of version v, as
// android-apk.yml names it: "minion-android-X.Y.Z[-rc.N].apk".
func APKAssetName(v SemVer) string {
	return "minion-android-" + strings.TrimPrefix(v.String(), "v") + ".apk"
}

// APKVersionCode is the Android versionCode of version v, the formula of
// android/app/build.gradle.kts: major*10_000_000 + minor*100_000 +
// patch*1_000 + rc, with rc 999 for a final release. A version outside the
// bounds of that formula has no code.
func APKVersionCode(v SemVer) (int64, error) {
	rc := int64(999)
	if v.Pre != "" {
		n, ok := strings.CutPrefix(v.Pre, "rc.")
		r, err := strconv.ParseInt(n, 10, 64)
		if !ok || err != nil || r < 1 || r > 998 || strconv.FormatInt(r, 10) != n {
			return 0, fmt.Errorf("%s has no Android version code (only -rc.1 to -rc.998)", v)
		}
		rc = r
	}
	if v.Major > 209 || v.Minor > 99 || v.Patch > 99 {
		return 0, fmt.Errorf("%s does not fit the Android version code", v)
	}
	return int64(v.Major)*10_000_000 + int64(v.Minor)*100_000 + int64(v.Patch)*1_000 + rc, nil
}

// APKOptions are the inputs of [Updater.StageAPK].
type APKOptions struct {
	// Tag is the exact release: "vX.Y.Z[-rc.N]" or "android-vX.Y.Z".
	Tag string
	// Current is the running version ("vX.Y.Z"); a development build cannot
	// stage an APK.
	Current string
	// CurrentCode is the versionCode of the installed app as the app reported
	// it; 0 means unknown, then the code of Current counts. The higher of the
	// two counts.
	CurrentCode int64
	// HubRange is the protocol range of the connected Hub; required.
	HubRange *ProtoRange
	// Dir is the staging directory in the app's private storage. It is
	// created with mode 0700 and must pass the trust check of the update
	// cache. Earlier staged APKs in it are removed.
	Dir string
}

// StagedAPK is a verified, staged APK.
type StagedAPK struct {
	// Tag is the release; Version its version and VersionCode the Android
	// versionCode the APK must carry.
	Tag         string
	Version     SemVer
	VersionCode int64
	// APKOnly is true for an "android-v" release; Base is then the full
	// release whose signed manifest gave the protocol range.
	APKOnly bool
	Base    string
	// Asset is the file name, SHA256 its digest from the signed checksums.txt
	// and Size its length.
	Asset  string
	SHA256 string
	Size   int64
	// Path is the staged APK (mode 0400). Checksums and Bundle are the signed
	// checksums.txt and its bundle, staged next to it so that a later check
	// ([VerifyStagedAPK]) can authenticate the APK again without a network.
	Path, Checksums, Bundle string
}

// Names of the staged files next to the APK.
const (
	stagedChecksums = "checksums.txt"
	stagedBundle    = "checksums.txt.sigstore.json"
)

// StageAPK downloads the APK of one exact release, verifies it and stages it
// in o.Dir. It refuses a version that is not newer than the installed one
// (semver and versionCode), a release whose protocol range does not overlap
// the Hub's, and any checksum or signature mismatch.
func (u *Updater) StageAPK(ctx context.Context, o APKOptions) (StagedAPK, error) {
	var res StagedAPK
	if u.Verifier == nil || u.Source == nil {
		return res, errors.New("update: no source or verifier configured")
	}
	v, apkOnly, err := ParseReleaseTag(o.Tag)
	if err != nil {
		return res, err
	}
	code, err := APKVersionCode(v)
	if err != nil {
		return res, err
	}
	res = StagedAPK{Tag: o.Tag, Version: v, VersionCode: code, APKOnly: apkOnly, Asset: APKAssetName(v)}
	if err := checkAPKNewer(o, v, code); err != nil {
		return res, err
	}
	if o.HubRange == nil {
		return res, errors.New("not connected to a Hub: the protocol range of the new release cannot be checked")
	}
	if o.Dir == "" {
		return res, errors.New("no staging directory for the APK")
	}
	releases, err := u.Source.Releases(ctx)
	if err != nil {
		return res, err
	}
	rel, ok := findRelease(releases, o.Tag)
	if !ok {
		return res, fmt.Errorf("%s is not published (missing or still a draft)", o.Tag)
	}
	if err := u.Cache.Prepare(); err != nil {
		return res, err
	}
	verifier, err := u.Verifier(ctx, false)
	if err != nil {
		return res, err
	}

	var checksumsRaw, bundleRaw []byte
	var sums Checksums
	if apkOnly {
		base, ok := apkBase(releases, v)
		if !ok {
			return res, fmt.Errorf("%s has no full release v%d.%d.* at or below it to take the protocol range from", o.Tag, v.Major, v.Minor)
		}
		res.Base = base.Tag
		bm, err := u.fetchMetadata(ctx, verifier, base)
		if err != nil {
			return res, fmt.Errorf("base release %s: %w", base.Tag, err)
		}
		if err := checkProtocol(bm.manifest, o.HubRange); err != nil {
			return res, fmt.Errorf("base release %s: %w", base.Tag, err)
		}
		for _, f := range []struct {
			name string
			dst  *[]byte
		}{{ChecksumsAsset, &checksumsRaw}, {BundleName(ChecksumsAsset), &bundleRaw}} {
			a, ok := rel.Asset(f.name)
			if !ok {
				return res, fmt.Errorf("release %s has no %s; it cannot be verified", rel.Tag, f.name)
			}
			if *f.dst, err = u.readAsset(ctx, a, maxManifestSize); err != nil {
				return res, err
			}
		}
		if err := verifier.VerifyBlob(checksumsRaw, bundleRaw, o.Tag); err != nil {
			return res, fmt.Errorf("%s: %w", ChecksumsAsset, err)
		}
		if sums, err = ParseChecksums(checksumsRaw); err != nil {
			return res, err
		}
	} else {
		meta, err := u.fetchMetadata(ctx, verifier, rel)
		if err != nil {
			return res, err
		}
		if err := checkProtocol(meta.manifest, o.HubRange); err != nil {
			return res, err
		}
		checksumsRaw, bundleRaw, sums = meta.checksumsRaw, meta.checksumsBundle, meta.checksums
		if md, listed := meta.manifest.Digest(res.Asset); listed && md != sums[res.Asset] {
			return res, fmt.Errorf("%w: release manifest and checksums.txt disagree about %s", ErrChecksum, res.Asset)
		}
	}
	want, ok := sums[res.Asset]
	if !ok {
		return res, fmt.Errorf("release %s lists no %s in its signed checksums", o.Tag, res.Asset)
	}
	asset, ok := rel.Asset(res.Asset)
	if !ok {
		return res, fmt.Errorf("release %s has no asset %s", o.Tag, res.Asset)
	}
	res.SHA256 = want

	if err := prepareAPKDir(o.Dir); err != nil {
		return res, err
	}
	part := filepath.Join(o.Dir, "."+res.Asset+".part")
	_ = os.Remove(part)
	f, err := u.download(ctx, asset, part, want)
	if err != nil {
		return res, err
	}
	fi, err := f.Stat()
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(part, 0o400)
	}
	final := filepath.Join(o.Dir, res.Asset)
	if err == nil {
		_ = os.Chmod(final, 0o600) // an earlier copy of the same release
		_ = os.Remove(final)
		err = os.Rename(part, final)
	}
	if err != nil {
		_ = os.Remove(part)
		return res, fmt.Errorf("cannot stage %s: %w", res.Asset, err)
	}
	res.Size = fi.Size()
	res.Path = final
	res.Checksums = filepath.Join(o.Dir, stagedChecksums)
	res.Bundle = filepath.Join(o.Dir, stagedBundle)
	if err := writeFileAtomic(res.Checksums, checksumsRaw, 0o400); err != nil {
		return res, err
	}
	if err := writeFileAtomic(res.Bundle, bundleRaw, 0o400); err != nil {
		return res, err
	}
	removeOtherAPKs(o.Dir, res.Asset)
	return res, nil
}

// checkAPKNewer refuses an APK that is not newer than the installed app, by
// version and by versionCode (Android refuses a lower code, Minion also an
// equal one).
func checkAPKNewer(o APKOptions, v SemVer, code int64) error {
	cur, err := ParseVersion(o.Current)
	if err != nil {
		return errors.New("this Worker is a development build; install the APK by hand")
	}
	curCode, err := APKVersionCode(cur)
	if err != nil {
		curCode = 0
	}
	if o.CurrentCode > curCode {
		curCode = o.CurrentCode
	}
	switch c := v.Compare(cur); {
	case c == 0 || (c > 0 && code == curCode):
		return fmt.Errorf("%w: %s", ErrUpToDate, cur)
	case c < 0 || code < curCode:
		return fmt.Errorf("%w from %s (version code %d) to %s (version code %d)", ErrDowngrade, cur, curCode, v, code)
	}
	return nil
}

// findRelease returns the published release with exactly this tag.
func findRelease(releases []Release, tag string) (Release, bool) {
	for _, r := range releases {
		if r.Tag == tag && !r.Draft {
			return r, true
		}
	}
	return Release{}, false
}

// apkBase is the base of an APK-only release of version v: the newest
// published final full release vX.Y.Z' with the same major and minor and
// Z' <= Z.
func apkBase(releases []Release, v SemVer) (Release, bool) {
	var best Release
	var bestVer SemVer
	found := false
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		rv, err := ParseVersion(r.Tag)
		if err != nil || rv.String() != r.Tag || rv.IsPrerelease() {
			continue
		}
		if rv.Major != v.Major || rv.Minor != v.Minor || rv.Compare(v) > 0 {
			continue
		}
		if !found || rv.Compare(bestVer) > 0 {
			best, bestVer, found = r, rv, true
		}
	}
	return best, found
}

// prepareAPKDir creates the staging directory and applies the trust check of
// the update cache to it: only this user may change it.
func prepareAPKDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create the APK staging directory: %w", err)
	}
	if err := checkTrustedDir(dir); err != nil {
		return fmt.Errorf("APK staging directory %q: %w", dir, err)
	}
	return nil
}

// removeOtherAPKs removes staged APKs of other releases and stale partial
// downloads.
func removeOtherAPKs(dir, keep string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if n == keep || !strings.HasSuffix(n, ".apk") && !strings.HasSuffix(n, ".apk.part") {
			continue
		}
		p := filepath.Join(dir, n)
		_ = os.Chmod(p, 0o600)
		_ = os.Remove(p)
	}
}

// RemoveStagedAPK removes everything StageAPK put in dir.
func RemoveStagedAPK(dir string) {
	removeOtherAPKs(dir, "")
	for _, n := range []string{stagedChecksums, stagedBundle} {
		p := filepath.Join(dir, n)
		_ = os.Chmod(p, 0o600)
		_ = os.Remove(p)
	}
}

// VerifiedAPK is what [VerifyStagedAPK] authenticated.
type VerifiedAPK struct {
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	VersionCode int64  `json:"version_code"`
	Asset       string `json:"asset"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Path        string `json:"path"`
}

// VerifyStagedAPK authenticates a staged APK again, offline: the staged
// checksums.txt must carry a valid signature of the release workflow for tag
// (Sigstore, with the cached trusted root), and the APK in dir must have the
// digest it lists. The app runs this (as "minion worker apk-verify", a fresh
// process from the read-only native library directory) to learn the digest it
// must see while it streams the APK into the install session: the staged
// files are writable by every process of the app's user, so neither a digest
// written next to them nor the file checked once is enough on its own.
func VerifyStagedAPK(ctx context.Context, verifiers VerifierFactory, dir, tag string) (VerifiedAPK, error) {
	v, _, err := ParseReleaseTag(tag)
	if err != nil {
		return VerifiedAPK{}, err
	}
	code, err := APKVersionCode(v)
	if err != nil {
		return VerifiedAPK{}, err
	}
	if verifiers == nil {
		return VerifiedAPK{}, errors.New("no signature verifier configured")
	}
	if err := checkTrustedDir(dir); err != nil {
		return VerifiedAPK{}, fmt.Errorf("APK staging directory %q: %w", dir, err)
	}
	sumsRaw, err := readTrustedFile(filepath.Join(dir, stagedChecksums), maxManifestSize)
	if err != nil {
		return VerifiedAPK{}, err
	}
	bundleRaw, err := readTrustedFile(filepath.Join(dir, stagedBundle), maxManifestSize)
	if err != nil {
		return VerifiedAPK{}, err
	}
	verifier, err := verifiers(ctx, true)
	if err != nil {
		return VerifiedAPK{}, err
	}
	if err := verifier.VerifyBlob(sumsRaw, bundleRaw, tag); err != nil {
		return VerifiedAPK{}, fmt.Errorf("%s: %w", ChecksumsAsset, err)
	}
	sums, err := ParseChecksums(sumsRaw)
	if err != nil {
		return VerifiedAPK{}, err
	}
	name := APKAssetName(v)
	want, ok := sums[name]
	if !ok {
		return VerifiedAPK{}, fmt.Errorf("the signed checksums of %s list no %s", tag, name)
	}
	p := filepath.Join(dir, name)
	f, err := openTrusted(p)
	if err != nil {
		return VerifiedAPK{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxArchiveSize+1))
	if err != nil {
		return VerifiedAPK{}, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return VerifiedAPK{}, fmt.Errorf("%w: the staged %s has sha256 %s, the signed checksums say %s", ErrChecksum, name, got, want)
	}
	return VerifiedAPK{Tag: tag, Version: v.String(), VersionCode: code, Asset: name, SHA256: want, Size: n, Path: p}, nil
}
