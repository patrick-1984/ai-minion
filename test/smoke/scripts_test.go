package smoke

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var shellScripts = []string{
	"common.sh", "systemd_container.sh", "fetch.sh", "channels.sh",
	"deb.sh", "rpm.sh", "bootstrap.sh", "image.sh", "macos.sh", "pin.sh", "promote.sh",
}

func TestScriptsParse(t *testing.T) {
	needTools(t, "bash")
	for _, s := range shellScripts {
		r := run(t, repoRoot(t), nil, "", "bash", "-n", filepath.Join("test", "smoke", s))
		if r.code != 0 {
			t.Errorf("bash -n %s: %s", s, r.all())
		}
	}
}

func TestShellcheck(t *testing.T) {
	needTools(t, "shellcheck")
	args := []string{"-x", "-S", "warning", "-e", "SC1091"}
	for _, s := range shellScripts {
		args = append(args, filepath.Join("test", "smoke", s))
	}
	if r := run(t, repoRoot(t), nil, "", "shellcheck", args...); r.code != 0 {
		t.Errorf("shellcheck: %s", r.all())
	}
}

func sh(t *testing.T, env []string, snippet string) result {
	t.Helper()
	needTools(t, "bash")
	src := filepath.Join(repoRoot(t), "test", "smoke", "common.sh")
	return run(t, repoRoot(t), env, "", "bash", "-c", "set -euo pipefail; . "+src+"; "+snippet)
}

func TestTagValidation(t *testing.T) {
	good := []string{"v0.1.0", "v1.4.2", "v10.20.30", "v0.1.0-rc.1", "v2.0.0-rc.12"}
	bad := []string{"", "0.1.0", "v1.2", "v1.2.3.4", "v01.2.3", "v1.2.3-rc.0", "v1.2.3-rc", "v1.2.3-beta.1", "v1.2.3-rc.1-x", "latest", "v1.2.3; rm -rf /"}
	for _, tag := range good {
		if r := sh(t, nil, "smoke_valid_tag "+shQuote(tag)); r.code != 0 {
			t.Errorf("%q must be valid", tag)
		}
	}
	for _, tag := range bad {
		if r := sh(t, nil, `smoke_valid_tag `+shQuote(tag)); r.code == 0 {
			t.Errorf("%q must be invalid", tag)
		}
	}
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestArchiveNames(t *testing.T) {
	cases := []struct{ tag, os, arch, want string }{
		{"v0.1.0", "linux", "amd64", "minion_0.1.0_linux_amd64.tar.gz"},
		{"v0.1.0", "darwin", "arm64", "minion_0.1.0_darwin_arm64.tar.gz"},
		{"v0.1.0-rc.1", "windows", "amd64", "minion_0.1.0-rc.1_windows_amd64_preview.zip"},
	}
	for _, c := range cases {
		r := sh(t, nil, "smoke_archive_name "+c.tag+" "+c.os+" "+c.arch)
		if strings.TrimSpace(r.stdout) != c.want {
			t.Errorf("archive(%s,%s,%s) = %q, want %q (%s)", c.tag, c.os, c.arch, r.stdout, c.want, r.stderr)
		}
	}
}

// --- channels.sh: the publish plan ------------------------------------------

func plan(t *testing.T, tag string, published ...string) map[string]string {
	t.Helper()
	r := script(t, nil, strings.Join(published, "\n")+"\n", "channels.sh", tag)
	if r.code != 0 {
		t.Fatalf("channels.sh %s: exit %d: %s", tag, r.code, r.all())
	}
	out := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("bad output line %q", l)
		}
		out[k] = v
	}
	return out
}

// AC: an -rc.N is published as a prerelease and moves no channel tag and no
// image tag other than its own (A-13).
func TestPlanReleaseCandidateMovesNothing(t *testing.T) {
	for _, tag := range []string{"v0.1.0-rc.1", "v1.4.2-rc.3"} {
		for _, published := range [][]string{nil, {"v0.0.9", "v1.4.1", "v1.3.0"}} {
			p := plan(t, tag, published...)
			if p["kind"] != "rc" || p["prerelease"] != "true" || p["github_latest"] != "false" || p["image_tags"] != "" {
				t.Errorf("plan(%s, %v) = %v; a candidate is a prerelease that moves no tag", tag, published, p)
			}
		}
	}
}

// AC: a final release moves image tags X.Y, X and latest.
func TestPlanFinalMovesChannels(t *testing.T) {
	p := plan(t, "v0.1.0")
	if p["kind"] != "final" || p["prerelease"] != "false" || p["github_latest"] != "true" || p["image_tags"] != "0.1 0 latest" {
		t.Errorf("first final release: %v", p)
	}
	p = plan(t, "v1.4.3", "v1.4.2", "v1.4.2-rc.1", "v1.3.0", "v0.9.0")
	if p["image_tags"] != "1.4 1 latest" || p["github_latest"] != "true" {
		t.Errorf("newest final release: %v", p)
	}
}

// A channel never moves backwards.
func TestPlanChannelsNeverMoveBackwards(t *testing.T) {
	cases := []struct {
		tag       string
		published []string
		tags      string
		latest    string
	}{
		{"v1.4.3", []string{"v1.5.0"}, "1.4", "false"},
		{"v1.4.3", []string{"v2.0.0"}, "1.4 1", "false"},
		{"v1.4.3", []string{"v1.4.4"}, "", "false"},
		{"v1.0.0", []string{"v10.0.0", "v1.4.2"}, "1.0", "false"}, // prefixes are not regexes: v10 is not v1.x
		{"v1.10.0", []string{"v1.9.0"}, "1.10 1 latest", "true"},
		{"v1.4.2", []string{"v1.4.2"}, "1.4 1 latest", "true"}, // a rerun
	}
	for _, c := range cases {
		p := plan(t, c.tag, c.published...)
		if p["image_tags"] != c.tags || p["github_latest"] != c.latest {
			t.Errorf("plan(%s, %v) = %v, want tags %q latest %s", c.tag, c.published, p, c.tags, c.latest)
		}
	}
}

func TestPlanRejectsBadTag(t *testing.T) {
	for _, tag := range []string{"latest", "v1.2", "1.2.3", "v1.2.3-rc"} {
		if r := script(t, nil, "", "channels.sh", tag); r.code == 0 || !strings.Contains(r.stderr, "not a release tag") {
			t.Errorf("channels.sh %q: exit %d, %q", tag, r.code, r.stderr)
		}
	}
}

// --- checksum and cosign verification ---------------------------------------

func releaseFiles() map[string]string {
	return map[string]string{
		"minion_0.1.0_linux_amd64.tar.gz": "archive-bytes",
		"minion_0.1.0_amd64.deb":          "deb-bytes",
		"bootstrap.sh":                    "#!/bin/sh\necho bootstrap\n",
	}
}

// AC: the smoke jobs verify checksums and the cosign bundle against the exact
// identity of the release workflow at refs/tags/<tag>.
func TestVerifyReleaseAssets(t *testing.T) {
	env, logPath := stubBin(t, map[string]string{"cosign": cosignStub})
	dir := fixtureAssets(t, releaseFiles())
	r := sh(t, env, "smoke_verify_release_assets "+shQuote(dir)+" v0.1.0 minion_0.1.0_linux_amd64.tar.gz minion_0.1.0_amd64.deb")
	if r.code != 0 {
		t.Fatalf("verification failed: %s", r.all())
	}
	calls := readLog(t, logPath)
	for _, want := range []string{
		"verify-blob",
		"--bundle " + dir + "/checksums.txt.sigstore.json",
		"--certificate-identity https://github.com/patrick-1984/ai-minion/.github/workflows/release.yml@refs/tags/v0.1.0",
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
		"--certificate-github-workflow-repository patrick-1984/ai-minion",
		"--certificate-github-workflow-ref refs/tags/v0.1.0",
		"--bundle " + dir + "/bootstrap.sh.sigstore.json", // signed on its own
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("cosign was not called with %q; calls:\n%s", want, calls)
		}
	}
}

func TestVerifyRejectsATamperedAsset(t *testing.T) {
	env, _ := stubBin(t, map[string]string{"cosign": cosignStub})
	dir := fixtureAssets(t, releaseFiles())
	writeFile(t, filepath.Join(dir, "minion_0.1.0_amd64.deb"), "tampered", 0o644)
	r := sh(t, env, "smoke_verify_release_assets "+shQuote(dir)+" v0.1.0 minion_0.1.0_amd64.deb")
	if r.code == 0 || !strings.Contains(r.stderr, "does not match the signed checksums.txt") {
		t.Fatalf("a tampered asset passed: exit %d %s", r.code, r.stderr)
	}
}

func TestVerifyRejectsABadSignature(t *testing.T) {
	env, _ := stubBin(t, map[string]string{"cosign": cosignStub})
	env = append(env, "COSIGN_RC=1")
	dir := fixtureAssets(t, releaseFiles())
	r := sh(t, env, "smoke_verify_release_assets "+shQuote(dir)+" v0.1.0 minion_0.1.0_amd64.deb")
	if r.code == 0 || !strings.Contains(r.stderr, "cosign bundle of checksums.txt does not verify") {
		t.Fatalf("a failing cosign passed: exit %d %s", r.code, r.stderr)
	}
}

func TestVerifyRejectsAMissingBundleOrAsset(t *testing.T) {
	env, _ := stubBin(t, map[string]string{"cosign": cosignStub})
	dir := fixtureAssets(t, releaseFiles())
	rmFile(t, filepath.Join(dir, "checksums.txt.sigstore.json"))
	if r := sh(t, env, "smoke_verify_release_assets "+shQuote(dir)+" v0.1.0 minion_0.1.0_amd64.deb"); r.code == 0 || !strings.Contains(r.stderr, "missing the cosign bundle") {
		t.Errorf("missing bundle: exit %d %s", r.code, r.stderr)
	}
	dir = fixtureAssets(t, releaseFiles())
	if r := sh(t, env, "smoke_verify_release_assets "+shQuote(dir)+" v0.1.0 nope.deb"); r.code == 0 || !strings.Contains(r.stderr, "missing asset nope.deb") {
		t.Errorf("missing asset: exit %d %s", r.code, r.stderr)
	}
	if r := sh(t, env, "smoke_verify_release_assets "+shQuote(dir)+" main minion_0.1.0_amd64.deb"); r.code == 0 {
		t.Errorf("a branch name was accepted as a tag")
	}
}

// GitHub may rename an asset ("~" becomes "."): the hash is compared, not the name.
func TestVerifyComparesHashesNotNames(t *testing.T) {
	env, _ := stubBin(t, map[string]string{"cosign": cosignStub})
	dir := fixtureAssets(t, map[string]string{"minion_0.1.0~rc.1_amd64.deb": "deb-bytes"})
	if err := renameFile(filepath.Join(dir, "minion_0.1.0~rc.1_amd64.deb"), filepath.Join(dir, "minion_0.1.0.rc.1_amd64.deb")); err != nil {
		t.Fatal(err)
	}
	r := sh(t, env, "smoke_verify_release_assets "+shQuote(dir)+" v0.1.0 minion_0.1.0.rc.1_amd64.deb")
	if r.code != 0 {
		t.Fatalf("renamed asset: %s", r.all())
	}
}

func TestExpectVersion(t *testing.T) {
	if r := sh(t, nil, `smoke_expect_version "$(printf 'version: v0.1.0\ncommit: abc\n')" v0.1.0`); r.code != 0 {
		t.Errorf("good version rejected: %s", r.stderr)
	}
	for _, out := range []string{"version: v0.1.1", "version: dev", "xversion: v0.1.0", ""} {
		if r := sh(t, nil, "smoke_expect_version "+shQuote(out)+" v0.1.0"); r.code == 0 {
			t.Errorf("version output %q accepted", out)
		}
	}
}

// --- fetch.sh ----------------------------------------------------------------

// ghStub copies the fixture assets named by --pattern into --dir, like
// "gh release download".
const ghStub = `printf 'gh %s\n' "$*" >>"$STUB_LOG"
[ "$1" = release ] && [ "$2" = download ] || exit 2
tag=$3; shift 3
dir=""; pats=()
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) dir=$2; shift 2 ;;
    --pattern) pats+=("$2"); shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$dir" ] || exit 2
mkdir -p "$dir"
shopt -s nullglob
for f in "$FIXTURE"/*; do
  b=$(basename "$f")
  if [ ${#pats[@]} -eq 0 ]; then cp "$f" "$dir/$b"; continue; fi
  for p in "${pats[@]}"; do
    case "$b" in $p) cp "$f" "$dir/$b"; break ;; esac
  done
done
`

func TestFetchDownloadsAndVerifies(t *testing.T) {
	env, logPath := stubBin(t, map[string]string{"cosign": cosignStub, "gh": ghStub})
	fixture := fixtureAssets(t, releaseFiles())
	out := t.TempDir()
	env = append(env, "FIXTURE="+fixture)
	r := script(t, env, "", "fetch.sh", testTag, out, "*_linux_amd64.tar.gz")
	if r.code != 0 {
		t.Fatalf("fetch.sh: exit %d %s", r.code, r.all())
	}
	calls := readLog(t, logPath)
	if !strings.Contains(calls, "gh release download v0.1.0 --repo patrick-1984/ai-minion") || !strings.Contains(calls, "--pattern *_linux_amd64.tar.gz") {
		t.Errorf("unexpected gh call:\n%s", calls)
	}
	if !strings.Contains(calls, "cosign verify-blob") {
		t.Errorf("fetch.sh did not verify the cosign bundle:\n%s", calls)
	}
	if !fileExists(filepath.Join(out, "minion_0.1.0_linux_amd64.tar.gz")) || fileExists(filepath.Join(out, "minion_0.1.0_amd64.deb")) {
		t.Errorf("fetch.sh must download only the matching assets")
	}
}

func TestFetchRefusesBadInput(t *testing.T) {
	env, _ := stubBin(t, map[string]string{"cosign": cosignStub, "gh": ghStub})
	fixture := fixtureAssets(t, releaseFiles())
	env = append(env, "FIXTURE="+fixture)

	if r := script(t, env, "", "fetch.sh", "main", t.TempDir()); r.code == 0 || !strings.Contains(r.stderr, "not a release tag") {
		t.Errorf("a branch name: exit %d %s", r.code, r.stderr)
	}
	// A tampered archive.
	writeFile(t, filepath.Join(fixture, "minion_0.1.0_linux_amd64.tar.gz"), "tampered", 0o644)
	if r := script(t, env, "", "fetch.sh", testTag, t.TempDir()); r.code == 0 || !strings.Contains(r.stderr, "does not match the signed checksums.txt") {
		t.Errorf("tampered archive: exit %d %s", r.code, r.stderr)
	}
	// A failing signature.
	fixture = fixtureAssets(t, releaseFiles())
	bad := append([]string{}, env...)
	bad = append(bad, "FIXTURE="+fixture, "COSIGN_RC=1")
	if r := script(t, bad, "", "fetch.sh", testTag, t.TempDir()); r.code == 0 || !strings.Contains(r.stderr, "does not verify") {
		t.Errorf("bad signature: exit %d %s", r.code, r.stderr)
	}
}

// The draft is pinned: a checksums.txt other than the one publish.yml resolved
// is refused before anything is installed (the draft was rebuilt in between).
func TestFetchRefusesAChecksumsTxtThatIsNotPinned(t *testing.T) {
	env, _ := stubBin(t, map[string]string{"cosign": cosignStub, "gh": ghStub})
	fixture := fixtureAssets(t, releaseFiles())
	env = append(env, "FIXTURE="+fixture)
	pin, err := os.ReadFile(filepath.Join(fixture, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}

	good := append(append([]string{}, env...), "SMOKE_CHECKSUMS_SHA256="+sha(string(pin)))
	if r := script(t, good, "", "fetch.sh", testTag, t.TempDir(), "*_linux_amd64.tar.gz"); r.code != 0 {
		t.Fatalf("the pinned checksums.txt was refused: exit %d %s", r.code, r.all())
	}
	other := append(append([]string{}, env...), "SMOKE_CHECKSUMS_SHA256="+sha("another checksums.txt"))
	out := t.TempDir()
	if r := script(t, other, "", "fetch.sh", testTag, out, "*_linux_amd64.tar.gz"); r.code == 0 || !strings.Contains(r.stderr, "the draft changed after it was resolved") {
		t.Errorf("a checksums.txt with another hash passed: exit %d %s", r.code, r.stderr)
	}
	for _, bad := range []string{"nothex", strings.ToUpper(sha("x")), sha("x")[:63]} {
		malformed := append(append([]string{}, env...), "SMOKE_CHECKSUMS_SHA256="+bad)
		if r := script(t, malformed, "", "fetch.sh", testTag, t.TempDir()); r.code == 0 || !strings.Contains(r.stderr, "64 lowercase hex digits") {
			t.Errorf("pin %q: exit %d %s", bad, r.code, r.stderr)
		}
	}
}

// --- pin.sh: right before the publish -------------------------------------------

// dockerPin answers "docker buildx imagetools inspect" with STUB_DIGEST.
const dockerPin = `printf 'docker %s\n' "$*" >>"$STUB_LOG"
[ "$1 $2" = "buildx imagetools" ] && echo "$STUB_DIGEST"
exit 0
`

// fullReleaseFiles is releaseFiles plus the signed release-manifest.json, the
// complete asset set of a real release.
func fullReleaseFiles() map[string]string {
	files := releaseFiles()
	files["release-manifest.json"] = `{"version":"v0.1.0"}`
	files["minion_0.1.0_linux_amd64.tar.gz.sbom.json"] = "sbom-bytes"
	return files
}

// assetManifest is the recorded asset set of dir, made by the real function.
func assetManifest(t *testing.T, dir string) string {
	t.Helper()
	r := sh(t, nil, "smoke_asset_manifest "+shQuote(dir))
	if r.code != 0 || r.stdout == "" {
		t.Fatalf("smoke_asset_manifest: %d %s", r.code, r.all())
	}
	return r.stdout
}

func TestPinChecksTheCompleteAssetSetAndTheImage(t *testing.T) {
	env, logPath := stubBin(t, map[string]string{"gh": ghStub, "docker": dockerPin, "cosign": cosignStub})
	fixture := fixtureAssets(t, fullReleaseFiles())
	manifest := filepath.Join(t.TempDir(), "assets.manifest")
	writeFile(t, manifest, assetManifest(t, fixture), 0o644)
	tested := stubDigest
	env = append(env, "FIXTURE="+fixture, "STUB_DIGEST="+tested)

	r := script(t, env, "", "pin.sh", testTag, manifest, tested)
	if r.code != 0 {
		t.Fatalf("pin.sh: exit %d %s", r.code, r.all())
	}
	calls := readLog(t, logPath)
	// Every asset is downloaded (no pattern) and verified before the publish.
	mustContain(t, "pin.sh calls", calls, "gh release download v0.1.0", "docker buildx imagetools inspect ghcr.io/patrick-1984/minion:v0.1.0", "cosign verify-blob")
	mustNotContain(t, "pin.sh calls", calls, "--pattern")

	// Each of these keeps checksums.txt byte for byte (the old pin) and must
	// still be refused.
	mutations := map[string]func(dir string){
		"a missing archive":               func(d string) { rmFile(t, filepath.Join(d, "minion_0.1.0_linux_amd64.tar.gz")) },
		"a replaced archive":              func(d string) { writeFile(t, filepath.Join(d, "minion_0.1.0_amd64.deb"), "other-bytes", 0o644) },
		"a replaced bootstrap.sh":         func(d string) { writeFile(t, filepath.Join(d, "bootstrap.sh"), "#!/bin/sh\necho evil\n", 0o644) },
		"a replaced release-manifest":     func(d string) { writeFile(t, filepath.Join(d, "release-manifest.json"), `{"version":"v9"}`, 0o644) },
		"a replaced bundle":               func(d string) { writeFile(t, filepath.Join(d, "bootstrap.sh.sigstore.json"), `{"x":1}`, 0o644) },
		"an added asset":                  func(d string) { writeFile(t, filepath.Join(d, "extra.txt"), "x", 0o644) },
		"a missing sbom":                  func(d string) { rmFile(t, filepath.Join(d, "minion_0.1.0_linux_amd64.tar.gz.sbom.json")) },
		"a missing release-manifest.json": func(d string) { rmFile(t, filepath.Join(d, "release-manifest.json")) },
	}
	for name, mutate := range mutations {
		changed := fixtureAssets(t, fullReleaseFiles())
		mutate(changed)
		sums, err := os.ReadFile(filepath.Join(changed, "checksums.txt"))
		if err != nil {
			t.Fatal(err)
		}
		orig, _ := os.ReadFile(filepath.Join(fixture, "checksums.txt"))
		if string(sums) != string(orig) {
			t.Fatalf("%s: the mutation must keep checksums.txt", name)
		}
		other := append(append([]string{}, env...), "FIXTURE="+changed)
		if r := script(t, other, "", "pin.sh", testTag, manifest, tested); r.code == 0 || !strings.Contains(r.stderr, "the draft changed after it was resolved") {
			t.Errorf("%s passed: exit %d %s", name, r.code, r.stderr)
		}
	}
	// The files are the recorded ones but a signature does not verify: the
	// release stays a draft.
	bad := append(append([]string{}, env...), "COSIGN_RC=1")
	if r := script(t, bad, "", "pin.sh", testTag, manifest, tested); r.code == 0 || !strings.Contains(r.stderr, "does not verify") {
		t.Errorf("a bad signature passed: exit %d %s", r.code, r.stderr)
	}
	// The image tag was pushed again: another digest.
	moved := append(append([]string{}, env...), "STUB_DIGEST=sha256:"+strings.Repeat("b", 64))
	if r := script(t, moved, "", "pin.sh", testTag, manifest, tested); r.code == 0 || !strings.Contains(r.stderr, "the image was pushed again") {
		t.Errorf("a moved image tag passed: exit %d %s", r.code, r.stderr)
	}
	// Bad arguments.
	empty := filepath.Join(t.TempDir(), "empty")
	writeFile(t, empty, "", 0o644)
	for _, args := range [][]string{
		{"main", manifest, tested},
		{testTag, filepath.Join(t.TempDir(), "absent"), tested},
		{testTag, empty, tested},
		{testTag, manifest, "latest"},
		{testTag, manifest, ""},
		{testTag, manifest},
	} {
		if r := script(t, env, "", "pin.sh", args...); r.code == 0 {
			t.Errorf("pin.sh %q was accepted", args)
		}
	}
}

func TestRequireSignedAssets(t *testing.T) {
	ok := fixtureAssets(t, fullReleaseFiles())
	if r := sh(t, nil, "smoke_require_signed_assets "+shQuote(ok)); r.code != 0 {
		t.Fatalf("a complete release was refused: %s", r.all())
	}
	for name, drop := range map[string]string{
		"checksums.txt":                   "checksums.txt",
		"the checksums bundle":            "checksums.txt.sigstore.json",
		"release-manifest.json":           "release-manifest.json",
		"the release-manifest bundle":     "release-manifest.json.sigstore.json",
		"the bootstrap bundle":            "bootstrap.sh.sigstore.json",
		"bootstrap.sh (its bundle stays)": "bootstrap.sh",
	} {
		dir := fixtureAssets(t, fullReleaseFiles())
		rmFile(t, filepath.Join(dir, drop))
		if r := sh(t, nil, "smoke_require_signed_assets "+shQuote(dir)); r.code == 0 {
			t.Errorf("a release without %s was accepted", name)
		}
	}
}

// --- the publish gate, simulated -----------------------------------------------

// The all-green job of smoke.yml is the gate that publish.yml needs. Run its
// real script with the results of failing, cancelled and skipped jobs: a
// failure anywhere must fail it, so "needs: smoke" leaves the draft unpublished.
func TestSmokeGateFailsWhenAnyJobFails(t *testing.T) {
	needTools(t, "bash")
	doc := jobBlock(t, readRepoFile(t, ".github/workflows/smoke.yml"), "smoke-passed")
	m := regexp.MustCompile(`(?m)^        run: \|\n((?:          .*\n?)+)`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("smoke-passed has no run script")
	}
	body := regexp.MustCompile(`(?m)^          `).ReplaceAllString(m[1], "")
	gate := func(results string) int {
		return run(t, repoRoot(t), []string{"RESULTS=" + results}, "", "bash", "-c", "set -e\n"+body).code
	}
	if code := gate("success success success success success success success"); code != 0 {
		t.Errorf("all jobs green: the gate exited %d", code)
	}
	for _, results := range []string{
		"success failure success", "failure", "success cancelled", "skipped success", "success success success success success success failure",
	} {
		if gate(results) == 0 {
			t.Errorf("the gate passed with the results %q", results)
		}
	}
}
