package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// apkRelease is one release of the fake APK host.
type apkRelease struct {
	tag      string
	apk      string // APK content; empty: no APK
	protoMin string // full releases: manifest range (default 1.0 - 1.1)
	protoMax string
	signAs   string // sign checksums.txt for this tag's identity (default tag)
	draft    bool
}

type apkHost struct {
	t        *testing.T
	sig      *fakeSigstore
	srv      *httptest.Server
	releases []apkRelease
	assets   map[string]map[string][]byte
	tamper   map[string][]byte // asset name -> content served instead
}

func newAPKHost(t *testing.T, rels ...apkRelease) *apkHost {
	t.Helper()
	h := &apkHost{t: t, sig: newFakeSigstore(t), releases: rels, assets: map[string]map[string][]byte{}, tamper: map[string][]byte{}}
	for _, r := range rels {
		h.assets[r.tag] = h.build(r)
	}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *apkHost) build(r apkRelease) map[string][]byte {
	v, apkOnly, err := ParseReleaseTag(r.tag)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string][]byte{}
	var sums strings.Builder
	name := APKAssetName(v)
	if r.apk != "" {
		out[name] = []byte(r.apk)
		s := sha256.Sum256([]byte(r.apk))
		sums.WriteString(hex.EncodeToString(s[:]) + "  " + name + "\n")
	}
	sums.WriteString(strings.Repeat("ab", 32) + "  other.tar.gz\n")
	signAs := r.signAs
	if signAs == "" {
		signAs = r.tag
	}
	spec := releaseSpec(signAs, time.Now())
	out[ChecksumsAsset] = []byte(sums.String())
	out[BundleName(ChecksumsAsset)] = h.sig.sign(h.t, out[ChecksumsAsset], spec)
	if !apkOnly {
		lo, hi := r.protoMin, r.protoMax
		if lo == "" {
			lo, hi = "1.0", "1.1"
		}
		m := manifestJSON(r.tag, lo, hi, `{"name":"other.tar.gz","sha256":"`+strings.Repeat("ab", 32)+`"}`)
		out[ManifestAsset] = m
		out[BundleName(ManifestAsset)] = h.sig.sign(h.t, m, spec)
	}
	return out
}

func (h *apkHost) serve(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/repos/"+RepoOwner+"/"+RepoName+"/releases":
		var out []ghRelease
		for _, r := range h.releases {
			gr := ghRelease{TagName: r.tag, Draft: r.draft}
			for name, data := range h.assets[r.tag] {
				gr.Assets = append(gr.Assets, ghAsset{Name: name, URL: h.srv.URL + "/dl/" + r.tag + "/" + name, Size: int64(len(data))})
			}
			out = append(out, gr)
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(req.URL.Path, "/dl/"):
		parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/dl/"), "/", 2)
		data, ok := h.assets[parts[0]][parts[1]]
		if !ok {
			http.NotFound(w, req)
			return
		}
		if alt, ok := h.tamper[parts[1]]; ok {
			data = alt
		}
		_, _ = w.Write(data)
	default:
		http.NotFound(w, req)
	}
}

func (h *apkHost) updater(t *testing.T) *Updater {
	src, err := NewGitHubSource(h.srv.URL, "minion-test")
	if err != nil {
		t.Fatal(err)
	}
	return &Updater{Source: src, Verifier: h.verifiers(t), Cache: Cache{Dir: filepath.Join(t.TempDir(), "cache")}}
}

func (h *apkHost) verifiers(t *testing.T) VerifierFactory {
	return SigstoreVerifierFactory(StaticTrustedRoot{Material: h.sig.material(t)})
}

func hubRange(t *testing.T, lo, hi string) *ProtoRange {
	a, err := ParseProtoVersion(lo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseProtoVersion(hi)
	if err != nil {
		t.Fatal(err)
	}
	return &ProtoRange{Min: a, Max: b}
}

func stage(t *testing.T, h *apkHost, o APKOptions) (StagedAPK, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if o.Dir == "" {
		o.Dir = filepath.Join(t.TempDir(), "apk")
	}
	if o.HubRange == nil {
		o.HubRange = hubRange(t, "1.0", "1.1")
	}
	return h.updater(t).StageAPK(ctx, o)
}

func TestParseReleaseTagAndVersionCode(t *testing.T) {
	for tag, want := range map[string]int64{
		"v0.24.0":         2_400_999,
		"v0.24.0-rc.2":    2_400_002,
		"android-v0.23.1": 2_301_999,
		"v1.2.3":          10_203_999,
	} {
		v, _, err := ParseReleaseTag(tag)
		if err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
		got, err := APKVersionCode(v)
		if err != nil || got != want {
			t.Errorf("%s: code %d, %v; want %d", tag, got, err, want)
		}
	}
	for _, bad := range []string{"android-v0.23.1-rc.1", "android-0.23.1", "0.24.0", "v0.24", "android-v01.2.3", "latest"} {
		if _, _, err := ParseReleaseTag(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := APKVersionCode(SemVer{Major: 1, Pre: "beta.1"}); err == nil {
		t.Error("a non-rc pre-release got a version code")
	}
	if _, err := APKVersionCode(SemVer{Major: 1, Minor: 100}); err == nil {
		t.Error("minor 100 got a version code")
	}
	if got := APKAssetName(SemVer{Major: 0, Minor: 24, Pre: "rc.1"}); got != "minion-android-0.24.0-rc.1.apk" {
		t.Errorf("asset = %s", got)
	}
}

func TestAndroidReleaseIdentityIsItsOwnWorkflow(t *testing.T) {
	id := ReleaseIdentity("android-v0.23.1")
	if id.SubjectAlternativeName != RepositoryURI+"/.github/workflows/android-release.yml@refs/tags/android-v0.23.1" || id.SourceRef != "refs/tags/android-v0.23.1" {
		t.Errorf("identity = %+v", id)
	}
	if got := ReleaseIdentity("v0.23.1").SubjectAlternativeName; !strings.Contains(got, "/release.yml@refs/tags/v0.23.1") {
		t.Errorf("full release identity = %s", got)
	}
}

func TestStageAPKFullRelease(t *testing.T) {
	h := newAPKHost(t, apkRelease{tag: "v0.24.0", apk: "apk 0.24.0"}, apkRelease{tag: "v0.23.0", apk: "apk 0.23.0"})
	dir := filepath.Join(t.TempDir(), "apk")
	// A staged APK of an earlier attempt is replaced.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "minion-android-0.23.5.apk"), "old", 0o400)
	res, err := stage(t, h, APKOptions{Tag: "v0.24.0", Current: "v0.23.0", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("apk 0.24.0"))
	if res.VersionCode != 2_400_999 || res.SHA256 != hex.EncodeToString(sum[:]) || res.Size != int64(len("apk 0.24.0")) || res.APKOnly ||
		res.Path != filepath.Join(dir, "minion-android-0.24.0.apk") {
		t.Errorf("staged = %+v", res)
	}
	if got := readFile(t, res.Path); got != "apk 0.24.0" {
		t.Errorf("content = %q", got)
	}
	if fi, err := os.Stat(res.Path); err != nil || fi.Mode().Perm() != 0o400 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "minion-android-0.23.5.apk")); !os.IsNotExist(err) {
		t.Errorf("old APK kept: %v", err)
	}
	// The app's check authenticates it again, offline.
	got, err := VerifyStagedAPK(context.Background(), h.verifiers(t), dir, "v0.24.0")
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != res.SHA256 || got.VersionCode != res.VersionCode || got.Path != res.Path || got.Size != res.Size {
		t.Errorf("verified = %+v", got)
	}
	// A swapped APK (same user, after staging) fails that check.
	_ = os.Chmod(res.Path, 0o600)
	writeFile(t, res.Path, "evil", 0o400)
	if _, err := VerifyStagedAPK(context.Background(), h.verifiers(t), dir, "v0.24.0"); !errors.Is(err, ErrChecksum) {
		t.Errorf("swapped APK: %v", err)
	}
	// Staged files of another release do not verify under this tag.
	if _, err := VerifyStagedAPK(context.Background(), h.verifiers(t), dir, "v0.24.1"); err == nil {
		t.Error("checksums of v0.24.0 verified for v0.24.1")
	}
	RemoveStagedAPK(dir)
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("left after removal: %v", ents)
	}
}

func TestStageAPKOnlyReleaseUsesBaseManifest(t *testing.T) {
	h := newAPKHost(t,
		apkRelease{tag: "v0.23.0", apk: "apk 0.23.0", protoMin: "1.0", protoMax: "1.1"},
		apkRelease{tag: "android-v0.23.1", apk: "apk 0.23.1"},
		apkRelease{tag: "v0.24.0", apk: "apk 0.24.0", protoMin: "2.0", protoMax: "2.0"},
	)
	res, err := stage(t, h, APKOptions{Tag: "android-v0.23.1", Current: "v0.23.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.APKOnly || res.Base != "v0.23.0" || res.VersionCode != 2_301_999 {
		t.Errorf("staged = %+v", res)
	}
	// The base decides compatibility: a Hub outside v0.23.0's range refuses it.
	if _, err := stage(t, h, APKOptions{Tag: "android-v0.23.1", Current: "v0.23.0", HubRange: hubRange(t, "2.0", "2.0")}); err == nil || !strings.Contains(err.Error(), "base release v0.23.0") {
		t.Errorf("incompatible base: %v", err)
	}
}

func TestStageAPKRefusals(t *testing.T) {
	h := newAPKHost(t,
		apkRelease{tag: "v0.23.0", apk: "apk 0.23.0"},
		apkRelease{tag: "v0.24.0", apk: "apk 0.24.0"},
		apkRelease{tag: "v0.25.0"},                                               // no APK
		apkRelease{tag: "android-v0.26.1", apk: "apk 0.26.1"},                    // no base v0.26.*
		apkRelease{tag: "android-v0.23.2", apk: "apk 0.23.2", signAs: "v0.23.2"}, // signed by release.yml
		apkRelease{tag: "v0.27.0", apk: "apk 0.27.0", signAs: "android-v0.27.0"}, // signed by android-release.yml
		apkRelease{tag: "v0.28.0", apk: "apk 0.28.0", protoMin: "3.0", protoMax: "3.0"},
		apkRelease{tag: "v0.29.0", apk: "apk 0.29.0", draft: true},
	)
	cases := []struct {
		name string
		o    APKOptions
		want string
		is   error
	}{
		{"downgrade", APKOptions{Tag: "v0.23.0", Current: "v0.24.0"}, "", ErrDowngrade},
		{"same", APKOptions{Tag: "v0.24.0", Current: "v0.24.0"}, "", ErrUpToDate},
		{"app reports a newer code", APKOptions{Tag: "v0.24.0", Current: "v0.23.0", CurrentCode: 2_500_999}, "", ErrDowngrade},
		{"dev build", APKOptions{Tag: "v0.24.0", Current: "dev"}, "development build", nil},
		{"no APK", APKOptions{Tag: "v0.25.0", Current: "v0.24.0"}, "lists no minion-android-0.25.0.apk", nil},
		{"no base", APKOptions{Tag: "android-v0.26.1", Current: "v0.24.0"}, "no full release v0.26.*", nil},
		{"wrong workflow for android tag", APKOptions{Tag: "android-v0.23.2", Current: "v0.23.0"}, "", ErrVerification},
		{"wrong workflow for full tag", APKOptions{Tag: "v0.27.0", Current: "v0.24.0"}, "", ErrVerification},
		{"protocol", APKOptions{Tag: "v0.28.0", Current: "v0.24.0"}, "protocol", nil},
		{"draft", APKOptions{Tag: "v0.29.0", Current: "v0.24.0"}, "not published", nil},
		{"no hub", APKOptions{Tag: "v0.24.0", Current: "v0.23.0", HubRange: nil}, "not connected", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := c.o
			if c.name == "no hub" {
				// stage() fills a range; call StageAPK directly.
				o.Dir = filepath.Join(t.TempDir(), "apk")
				_, err := h.updater(t).StageAPK(context.Background(), o)
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("err = %v", err)
				}
				return
			}
			_, err := stage(t, h, o)
			if err == nil {
				t.Fatal("accepted")
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Errorf("err = %v, want %v", err, c.is)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestStageAPKTamperedDownloadLeavesNothing(t *testing.T) {
	h := newAPKHost(t, apkRelease{tag: "v0.24.0", apk: "apk 0.24.0"})
	h.tamper["minion-android-0.24.0.apk"] = []byte("apk 0.24.0 but changed")
	dir := filepath.Join(t.TempDir(), "apk")
	if _, err := stage(t, h, APKOptions{Tag: "v0.24.0", Current: "v0.23.0", Dir: dir}); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("left behind: %v", ents)
	}
}
