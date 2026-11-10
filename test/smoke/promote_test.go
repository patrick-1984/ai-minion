package smoke

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake release world for promote.sh: gh, docker, cosign and curl are stubs
// that read and write a state directory, so that the tests can inject a
// failure at every fallible step and look at what is left behind.
//
//	rel/<tag>/draft, prerelease, assets/*   the releases
//	public                                  tags of the public final releases
//	latest                                  the GitHub "latest" release
//	tags/<name>                             the digest of every image tag
//	fail                                    failure keys (one per line)
//	create_fail_after                       "imagetools create" fails after N tags
const worldGh = `printf 'gh %s\n' "$*" >>"$STUB_LOG"
failing() { [ -f "$WORLD/fail" ] && grep -qxF "$1" "$WORLD/fail"; }
rel() { printf '%s/rel/%s' "$WORLD" "$1"; }
case "$1 $2" in
"release list") cat "$WORLD/public" 2>/dev/null || true; exit 0 ;;
"release download")
  tag=$3; shift 3; dir=""
  while [ $# -gt 0 ]; do case "$1" in --dir) dir=$2; shift 2 ;; *) shift ;; esac; done
  mkdir -p "$dir"; cp "$(rel "$tag")"/assets/* "$dir"/; exit 0 ;;
"release edit")
  tag=$3; shift 3
  draft=""; pre=""; lat=""
  for a in "$@"; do case "$a" in --draft=*) draft=${a#*=} ;; --prerelease=*) pre=${a#*=} ;; --latest=*) lat=${a#*=} ;; esac; done
  if [ "$draft" = false ] && failing publish; then exit 1; fi
  if [ "$draft" = true ] && failing unpublish; then exit 1; fi
  if [ -z "$draft" ] && [ "$lat" = true ] && failing relatest; then exit 1; fi
  [ -z "$draft" ] || echo "$draft" >"$(rel "$tag")/draft"
  [ -z "$pre" ] || echo "$pre" >"$(rel "$tag")/prerelease"
  [ "$lat" != true ] || echo "$tag" >"$WORLD/latest"
  touch "$WORLD/public"
  if [ "$draft" = false ] && [ "$pre" = false ]; then grep -qxF "$tag" "$WORLD/public" || echo "$tag" >>"$WORLD/public"; fi
  if [ "$draft" = true ]; then grep -vxF "$tag" "$WORLD/public" >"$WORLD/public.new" || true; mv "$WORLD/public.new" "$WORLD/public"; fi
  if [ "$draft" = false ] && failing publish-after; then exit 1; fi
  exit 0 ;;
esac
if [ "$1" = api ]; then
  path=$2; shift 2
  case "$path" in
  */releases/latest)
    if failing latest-read; then echo "gh: Internal Server Error (HTTP 500)" >&2; exit 1; fi
    if [ -s "$WORLD/latest" ]; then cat "$WORLD/latest"; exit 0; fi
    echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  */releases)
    tag=$(printf '%s\n' "$*" | sed -n 's/.*tag_name == "\([^"]*\)".*/\1/p')
    [ -d "$(rel "$tag")" ] || exit 0
    if printf '%s' "$*" | grep -q 'assets\[\]'; then
      if failing extra-asset; then echo surprise.txt; fi
      ls "$(rel "$tag")/assets"; exit 0
    fi
    printf '%s\t%s\n' "$(cat "$(rel "$tag")/draft")" "$(cat "$(rel "$tag")/prerelease")"; exit 0 ;;
  esac
fi
exit 2
`

const worldCurl = `printf 'curl %s\n' "$*" >>"$STUB_LOG"
failing() { [ -f "$WORLD/fail" ] && grep -qxF "$1" "$WORLD/fail"; }
out=""; url=""
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done
failing curl && exit 22
name=${url##*/}; rest=${url%/*}; tag=${rest##*/}
[ "$(cat "$WORLD/rel/$tag/draft")" = false ] || exit 22
cp "$WORLD/rel/$tag/assets/$name" "$out"
if failing curl-tamper && [ "$name" = bootstrap.sh ]; then echo tampered >"$out"; fi
`

const worldCosign = `printf 'cosign %s\n' "$*" >>"$STUB_LOG"
failing() { [ -f "$WORLD/fail" ] && grep -qxF "$1" "$WORLD/fail"; }
case "$1:$2" in
verify-blob:*) failing cosign-blob && exit 1 ;;
verify:*@sha256:*) failing cosign-digest && exit 1 ;;
verify:*) failing cosign-tag && exit 1 ;;
esac
exit 0
`

const worldDocker = `printf 'docker %s\n' "$*" >>"$STUB_LOG"
failing() { [ -f "$WORLD/fail" ] && grep -qxF "$1" "$WORLD/fail"; }
[ "$1 $2" = "buildx imagetools" ] || exit 0
case "$3" in
inspect)
  ref=$4; tag=${ref##*:}
  if failing inspect-error; then echo "ERROR: dial tcp: network is unreachable" >&2; exit 1; fi
  if [ -f "$WORLD/tags/$tag" ]; then cat "$WORLD/tags/$tag"; exit 0; fi
  echo "ERROR: $ref: not found" >&2; exit 1 ;;
create)
  shift 3; n=0; names=(); src=""
  while [ $# -gt 0 ]; do case "$1" in --tag) names+=("${2##*:}"); shift 2 ;; *) src=$1; shift ;; esac; done
  digest=${src#*@}
  limit=$(cat "$WORLD/create_fail_after" 2>/dev/null || echo -1)
  for t in "${names[@]}"; do
    if [ "$limit" -ge 0 ] && [ "$n" -ge "$limit" ]; then echo "ERROR: registry unavailable" >&2; exit 1; fi
    mkdir -p "$WORLD/tags"; echo "$digest" >"$WORLD/tags/$t"; n=$((n + 1))
  done ;;
esac
exit 0
`

var (
	dOld   = "sha256:" + strings.Repeat("1", 64)
	dOlder = "sha256:" + strings.Repeat("2", 64)
	d123   = "sha256:" + strings.Repeat("a", 64)
	d124   = "sha256:" + strings.Repeat("b", 64)
)

type world struct {
	t   *testing.T
	dir string
	env []string
	log string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	needTools(t, "bash")
	env, logPath := stubBin(t, map[string]string{"gh": worldGh, "docker": worldDocker, "cosign": worldCosign, "curl": worldCurl})
	w := &world{t: t, dir: t.TempDir(), log: logPath}
	w.env = append(env, "WORLD="+w.dir, "GH_TOKEN=unused")
	for _, d := range []string{"rel", "tags"} {
		if err := os.MkdirAll(filepath.Join(w.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// release adds a release with its complete asset set.
func (w *world) release(tag string, draft, prerelease bool) {
	w.t.Helper()
	files := fullReleaseFiles()
	files["bootstrap.sh"] = "#!/bin/sh\necho " + tag + "\n"
	assets := fixtureAssets(w.t, files)
	root := filepath.Join(w.dir, "rel", tag)
	if err := os.MkdirAll(root, 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Rename(assets, filepath.Join(root, "assets")); err != nil {
		w.t.Fatal(err)
	}
	w.put("rel/"+tag+"/draft", boolStr(draft))
	w.put("rel/"+tag+"/prerelease", boolStr(prerelease))
	if !draft && !prerelease {
		w.appendLine("public", tag)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (w *world) put(rel, content string) {
	w.t.Helper()
	writeFile(w.t, filepath.Join(w.dir, rel), content+"\n", 0o644)
}

func (w *world) appendLine(rel, line string) {
	w.t.Helper()
	p := filepath.Join(w.dir, rel)
	b, _ := os.ReadFile(p)
	writeFile(w.t, p, string(b)+line+"\n", 0o644)
}

func (w *world) get(rel string) string {
	b, err := os.ReadFile(filepath.Join(w.dir, rel))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (w *world) fail(keys ...string) { w.put("fail", strings.Join(keys, "\n")) }

func (w *world) manifest(tag string) string {
	p := filepath.Join(w.dir, tag+".manifest")
	writeFile(w.t, p, assetManifest(w.t, filepath.Join(w.dir, "rel", tag, "assets")), 0o644)
	return p
}

func (w *world) promote(tag, digest string) result {
	w.t.Helper()
	return script(w.t, w.env, "", "promote.sh", tag, w.manifest(tag), digest)
}

func (w *world) isDraft(tag string) bool { return w.get("rel/"+tag+"/draft") == "true" }
func (w *world) imageTag(name string) string {
	return w.get("tags/" + name)
}

// channels sets the three image channels of the 1.2 line and "latest".
func (w *world) channels(digest string) {
	for _, n := range []string{"1.2", "1", "latest"} {
		w.put("tags/"+n, digest)
	}
}

func (w *world) calls() string { return readLog(w.t, w.log) }

func (w *world) expectChannels(digest string) {
	w.t.Helper()
	for _, n := range []string{"1.2", "1", "latest"} {
		if got := w.imageTag(n); got != digest {
			w.t.Errorf("image tag %s is %q, want %q", n, got, digest)
		}
	}
}

// A final release that nobody races: public, latest, channels moved and
// verified, nothing rolled back.
func TestPromoteFinalPublishesAndMovesChannels(t *testing.T) {
	w := newWorld(t)
	w.release("v1.2.2", false, false)
	w.release("v1.2.3", true, false)
	w.put("latest", "v1.2.2")
	w.channels(dOld)
	r := w.promote("v1.2.3", d123)
	if r.code != 0 {
		t.Fatalf("promote: exit %d %s", r.code, r.all())
	}
	if w.isDraft("v1.2.3") || w.get("latest") != "v1.2.3" {
		t.Errorf("draft=%v latest=%q", w.isDraft("v1.2.3"), w.get("latest"))
	}
	w.expectChannels(d123)
	calls := w.calls()
	mustContain(t, "calls", calls, "--draft=false --prerelease=false --latest=true", "cosign verify ghcr.io/patrick-1984/minion:1.2 ", "curl -fsSL")
	// Everything that can fail before a change runs before the publication.
	pub := strings.Index(calls, "release edit v1.2.3")
	for _, before := range []string{"cosign verify ghcr.io/patrick-1984/minion@" + d123, "api repos/patrick-1984/ai-minion/releases/latest", "imagetools inspect ghcr.io/patrick-1984/minion:1.2 "} {
		if i := strings.Index(calls, before); i < 0 || i > pub {
			t.Errorf("%q must run before the release is published (at %d, publish at %d)", before, i, pub)
		}
	}
	if strings.Contains(calls, "--draft=true") {
		t.Errorf("a rollback ran on success:\n%s", calls)
	}
}

// AC (A-13): a candidate is a public prerelease and moves no channel and no
// image tag, and does not take over "latest".
func TestPromoteCandidateMovesNothing(t *testing.T) {
	w := newWorld(t)
	w.release("v1.2.2", false, false)
	w.release("v1.3.0-rc.1", true, true)
	w.put("latest", "v1.2.2")
	w.channels(dOld)
	r := w.promote("v1.3.0-rc.1", d123)
	if r.code != 0 {
		t.Fatalf("promote: exit %d %s", r.code, r.all())
	}
	if w.isDraft("v1.3.0-rc.1") || w.get("rel/v1.3.0-rc.1/prerelease") != "true" || w.get("latest") != "v1.2.2" {
		t.Errorf("draft=%v latest=%q", w.isDraft("v1.3.0-rc.1"), w.get("latest"))
	}
	w.expectChannels(dOld)
	mustNotContain(t, "calls", w.calls(), "imagetools create", "--latest=true")
}

// Failure injection: every failure leaves the previous release and channel
// state, and nothing is changed when the failure comes before the publication.
func TestPromoteFailuresRestoreThePreviousState(t *testing.T) {
	cases := []struct {
		name string
		fail []string
		// after is how many image tags the registry accepts before it fails.
		createAfter string
		// changed says whether the run may have touched anything at all.
		beforePublish bool
		wantErr       string
	}{
		{name: "the image signature does not verify", fail: []string{"cosign-digest"}, beforePublish: true, wantErr: "does not verify"},
		{name: "the latest release cannot be read", fail: []string{"latest-read"}, beforePublish: true, wantErr: "latest release"},
		{name: "an image tag cannot be read (not a missing tag)", fail: []string{"inspect-error"}, beforePublish: true, wantErr: "current digest"},
		{name: "the publication is refused", fail: []string{"publish"}, wantErr: ""},
		{name: "the publication fails after it took effect", fail: []string{"publish-after"}, wantErr: ""},
		{name: "the public download fails", fail: []string{"curl"}, wantErr: "could not download"},
		{name: "the public release has an extra asset", fail: []string{"extra-asset"}, wantErr: "other assets"},
		{name: "a public asset differs from the tested one", fail: []string{"curl-tamper"}, wantErr: "tested ones"},
		{name: "a public signature does not verify", fail: []string{"cosign-blob"}, wantErr: "does not verify"},
		{name: "the registry fails after the first tag", createAfter: "1", wantErr: ""},
		{name: "the registry fails at the first tag", createAfter: "0", wantErr: ""},
		{name: "a moved tag fails its signature check", fail: []string{"cosign-tag"}, wantErr: "does not verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.release("v1.2.2", false, false)
			w.release("v1.2.3", true, false)
			w.put("latest", "v1.2.2")
			w.channels(dOld)
			w.put("tags/1.2", dOlder) // the channels differ, each must return to its own digest
			if len(tc.fail) > 0 {
				w.fail(tc.fail...)
			}
			if tc.createAfter != "" {
				w.put("create_fail_after", tc.createAfter)
			}
			r := w.promote("v1.2.3", d123)
			if r.code == 0 {
				t.Fatalf("promote succeeded:\n%s", r.all())
			}
			if tc.wantErr != "" && !strings.Contains(r.stderr, tc.wantErr) {
				t.Errorf("stderr lacks %q:\n%s", tc.wantErr, r.stderr)
			}
			if !w.isDraft("v1.2.3") {
				t.Errorf("the release is public after a failed promotion")
			}
			if got := w.get("latest"); got != "v1.2.2" {
				t.Errorf("latest is %q, want v1.2.2", got)
			}
			if got := w.get("public"); got != "v1.2.2" {
				t.Errorf("public releases: %q", got)
			}
			if got := w.get("rel/v1.2.3/prerelease"); got != "false" {
				t.Errorf("prerelease flag %q", got)
			}
			if got := w.imageTag("1.2"); got != dOlder {
				t.Errorf("1.2 is %q, want %q", got, dOlder)
			}
			for _, n := range []string{"1", "latest"} {
				if got := w.imageTag(n); got != dOld {
					t.Errorf("%s is %q, want %q", n, got, dOld)
				}
			}
			calls := w.calls()
			if tc.beforePublish && (strings.Contains(calls, "release edit") || strings.Contains(calls, "imagetools create")) {
				t.Errorf("a preflight failure changed something:\n%s", calls)
			}
			if !tc.beforePublish && tc.createAfter == "" && len(tc.fail) > 0 && tc.fail[0] != "publish" {
				// The failure came after the publication: the rollback ran.
				mustContain(t, "stderr", r.stderr, "rolling back")
			}
		})
	}
}

// A release that was public before the run is never unpublished; only the
// channels return.
func TestPromoteOfAnAlreadyPublicReleaseOnlyRestoresChannels(t *testing.T) {
	w := newWorld(t)
	w.release("v1.2.3", false, false)
	w.put("latest", "v1.2.3")
	w.channels(dOld)
	w.put("create_fail_after", "1")
	r := w.promote("v1.2.3", d123)
	if r.code == 0 {
		t.Fatalf("promote succeeded:\n%s", r.all())
	}
	if w.isDraft("v1.2.3") || w.get("latest") != "v1.2.3" {
		t.Errorf("a public release was touched: draft=%v latest=%q", w.isDraft("v1.2.3"), w.get("latest"))
	}
	w.expectChannels(dOld)
	mustNotContain(t, "calls", w.calls(), "--draft")
}

// A rollback step that fails is reported and does not hide the failure.
func TestPromoteReportsAnIncompleteRollback(t *testing.T) {
	w := newWorld(t)
	w.release("v1.2.3", true, false)
	w.channels(dOld)
	w.fail("curl", "unpublish")
	r := w.promote("v1.2.3", d123)
	if r.code == 0 || !strings.Contains(r.stderr, "ROLLBACK INCOMPLETE") {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
}

// The first release of a line: its channels did not exist and a registry cannot
// delete one tag. The rollback says so instead of pretending.
func TestPromoteRollbackSaysWhatItCannotUndo(t *testing.T) {
	w := newWorld(t)
	w.release("v1.2.3", true, false)
	w.fail("cosign-tag")
	r := w.promote("v1.2.3", d123)
	if r.code == 0 || !strings.Contains(r.stderr, "did not exist before") {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	if !w.isDraft("v1.2.3") {
		t.Error("the release is public")
	}
}

// Overlapping promotions (the publish workflow serializes them with the
// "publish-channels" group; the plan is read from the live state inside it).
// Whichever finishes last, a channel never moves backwards and "latest" stays
// with the newest final release.
func TestOverlappingPromotionsNeverMoveChannelsBackwards(t *testing.T) {
	for _, order := range [][]string{{"v1.2.4", "v1.2.3"}, {"v1.2.3", "v1.2.4"}} {
		t.Run(strings.Join(order, "_then_"), func(t *testing.T) {
			w := newWorld(t)
			w.release("v1.2.2", false, false)
			w.release("v1.2.3", true, false)
			w.release("v1.2.4", true, false)
			w.put("latest", "v1.2.2")
			w.channels(dOld)
			digests := map[string]string{"v1.2.3": d123, "v1.2.4": d124}
			for _, tag := range order {
				if r := w.promote(tag, digests[tag]); r.code != 0 {
					t.Fatalf("promote %s: exit %d %s", tag, r.code, r.all())
				}
			}
			w.expectChannels(d124)
			if got := w.get("latest"); got != "v1.2.4" {
				t.Errorf("latest is %q, want v1.2.4", got)
			}
			for _, tag := range order {
				if w.isDraft(tag) {
					t.Errorf("%s is still a draft", tag)
				}
			}
			// The older release was published without taking over "latest".
			if order[1] == "v1.2.3" {
				mustContain(t, "calls", w.calls(), "release edit v1.2.3 --repo patrick-1984/ai-minion --draft=false --prerelease=false --latest=false")
			}
		})
	}
}

// An older line is fixed after a newer minor shipped: its own channel moves,
// the newer lines keep theirs.
func TestOverlappingPromotionOfAnOlderLine(t *testing.T) {
	w := newWorld(t)
	w.release("v1.3.0", false, false)
	w.release("v1.2.5", true, false)
	w.put("latest", "v1.3.0")
	w.put("tags/1.3", d124)
	w.put("tags/1", d124)
	w.put("tags/latest", d124)
	w.put("tags/1.2", dOld)
	if r := w.promote("v1.2.5", d123); r.code != 0 {
		t.Fatalf("promote: exit %d %s", r.code, r.all())
	}
	if got := w.imageTag("1.2"); got != d123 {
		t.Errorf("1.2 is %q", got)
	}
	for _, n := range []string{"1.3", "1", "latest"} {
		if got := w.imageTag(n); got != d124 {
			t.Errorf("%s moved backwards to %q", n, got)
		}
	}
	if got := w.get("latest"); got != "v1.3.0" {
		t.Errorf("latest is %q", got)
	}
}
