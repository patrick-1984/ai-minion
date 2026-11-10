package smoke

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const stubDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// dockerImage emulates the docker commands of image.sh.
const dockerImage = `printf 'docker %s\n' "$*" >>"$STUB_LOG"
if [ "$1 $2" = "buildx imagetools" ]; then echo "${STUB_DIGEST:-` + stubDigest + `}"; exit 0; fi
case "$1" in
  port) echo "0.0.0.0:32768"; exit 0 ;;
  inspect) echo "${STUB_RUNNING:-true}"; exit 0 ;;
  logs) echo "stub log"; exit 0 ;;
  run)
    case "$*" in
      *unprepared-parent/data*)
        case "${STUB_UNPREPARED:-exact}" in
          exact) echo "Error: data dir /data is not writable by uid 65532; run: chown 65532:65532 <host dir> (permission denied)" >&2; exit 1 ;;
          wrong) echo "Error: permission denied" >&2; exit 1 ;;
          started) exit 0 ;;
          hang) sleep 5; exit 124 ;;
        esac ;;
    esac ;;
esac
exit 0
`

const curlStub = `printf 'curl %s\n' "$*" >>"$STUB_LOG"
printf '%s' "${STUB_HTTP:-200}"
`

func imageEnv(t *testing.T, extra ...string) (env []string, logPath string) {
	t.Helper()
	env, logPath = stubBin(t, map[string]string{
		"docker": dockerImage, "curl": curlStub, "cosign": cosignStub, "sleep": "exit 0",
	})
	return append(env, extra...), logPath
}

// AC: pull the image by digest and start "hub serve" as UID 65532 on a fresh
// bind-mounted data dir prepared with chown 65532:65532 (health 200), on a data
// dir restored with "minion hub restore" (health 200), and on an unprepared
// root-owned dir with the exact fix in the exit message (A-14).
func TestImageSmoke(t *testing.T) {
	env, logPath := imageEnv(t)
	r := script(t, env, "", "image.sh", testTag)
	if r.code != 0 {
		t.Fatalf("image.sh: exit %d %s", r.code, r.all())
	}
	calls := readLog(t, logPath)
	lines := strings.Split(calls, "\n")
	index := func(sub string, from int) int {
		for i := from; i < len(lines); i++ {
			if strings.Contains(lines[i], sub) {
				return i
			}
		}
		return -1
	}
	count := func(sub string) int { return strings.Count(calls, sub) }

	ref := "ghcr.io/patrick-1984/minion@" + stubDigest
	for _, want := range []string{
		"docker buildx imagetools inspect ghcr.io/patrick-1984/minion:v0.1.0",
		"docker pull " + ref,
		"cosign verify " + ref + " --certificate-identity https://github.com/patrick-1984/ai-minion/.github/workflows/release.yml@refs/tags/v0.1.0 --certificate-oidc-issuer https://token.actions.githubusercontent.com",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q; calls:\n%s", want, calls)
		}
	}
	// The data dirs are prepared with the documented chown (fresh, backup, restored).
	if n := count("chown 65532:65532 /d"); n != 3 {
		t.Errorf("chown 65532:65532 ran %d times, want 3", n)
	}
	// Every Hub container runs as 65532 with the image by digest.
	hubRuns := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "docker run") && strings.Contains(l, ref) {
			hubRuns++
			if !strings.Contains(l, "-u 65532:65532") {
				t.Errorf("a container of the image does not run as 65532: %s", l)
			}
		}
	}
	// fresh serve, backup, restore, restored serve, unprepared serve.
	if hubRuns != 5 {
		t.Errorf("%d containers of the image ran, want 5", hubRuns)
	}
	// Order: serve (fresh), backup, restore, serve (restored), unprepared.
	fresh := index("docker run -d --name minion-smoke-fresh", 0)
	backup := index("hub backup --out /backup/hub.tar.gz", fresh)
	restore := index("hub restore /backup/hub.tar.gz", backup)
	restored := index("docker run -d --name minion-smoke-restored", restore)
	unprep := index("unprepared-parent/data:/data", restored)
	if fresh < 0 || backup < 0 || restore < 0 || restored < 0 || unprep < 0 {
		t.Fatalf("scenarios missing or out of order: fresh %d backup %d restore %d restored %d unprepared %d\n%s", fresh, backup, restore, restored, unprep, calls)
	}
	if count("/healthz") != 2 {
		t.Errorf("health was probed %d times, want once per started Hub (2)", count("/healthz"))
	}
	if !regexp.MustCompile(`curl .*https://127\.0\.0\.1:32768/healthz`).MatchString(calls) {
		t.Errorf("the health probe does not use the mapped port:\n%s", calls)
	}
	if strings.Contains(calls, "MINION_ADMIN_PASSWORD=") {
		t.Error("the admin password must not be passed in argv")
	}
	if !strings.Contains(calls, "MINION_ADMIN_PASSWORD_FILE=/run/secrets/admin.pw") {
		t.Error("the admin password must come from a file")
	}
}

func TestImageSmokeFailures(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"a Hub that starts on an unprepared dir", []string{"STUB_UNPREPARED=started"}, "must refuse"},
		{"a wrong exit message", []string{"STUB_UNPREPARED=wrong"}, "does not print the exact fix"},
		{"a Hub that keeps running on an unprepared dir", []string{"STUB_UNPREPARED=hang"}, "must exit"},
		{"a Hub that never answers", []string{"STUB_HTTP=503", "STUB_RUNNING=false"}, "did not answer /healthz with 200"},
		{"an image without a valid signature", []string{"COSIGN_RC=1"}, "cosign signature"},
		{"an unresolvable digest", []string{"STUB_DIGEST=nonsense"}, "could not resolve the digest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, _ := imageEnv(t, c.env...)
			r := script(t, env, "", "image.sh", testTag)
			if r.code == 0 || !strings.Contains(r.all(), c.want) {
				t.Fatalf("expected failure containing %q, got exit %d: %s", c.want, r.code, r.all())
			}
		})
	}
	env, _ := imageEnv(t)
	if r := script(t, env, "", "image.sh", "latest"); r.code == 0 || !strings.Contains(r.all(), "not a release tag") {
		t.Errorf("a channel name was accepted as a tag: %d %s", r.code, r.all())
	}
}

// The digest that was verified and tested is handed to the publish gate, and
// only after every check passed.
func TestImageSmokeReportsTheTestedDigest(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output")
	env, _ := imageEnv(t, "SMOKE_DIGEST_OUTPUT="+out)
	if r := script(t, env, "", "image.sh", testTag); r.code != 0 {
		t.Fatalf("image.sh: exit %d %s", r.code, r.all())
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "digest="+stubDigest+"\n" {
		t.Errorf("digest output = %q", got)
	}

	failed := filepath.Join(t.TempDir(), "output")
	env, _ = imageEnv(t, "SMOKE_DIGEST_OUTPUT="+failed, "STUB_UNPREPARED=started")
	if r := script(t, env, "", "image.sh", testTag); r.code == 0 {
		t.Fatal("the smoke test passed although the Hub started on an unprepared dir")
	}
	if fileExists(failed) {
		t.Error("a digest was reported for an image that failed its smoke test")
	}
}
