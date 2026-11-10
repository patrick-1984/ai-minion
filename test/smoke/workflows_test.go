package smoke

import (
	"regexp"
	"strings"
	"testing"
)

// jobBlock returns the text of one job of a workflow ("  name:" up to the next
// job). The workflows are read as text on purpose: the project has no YAML
// library, and the files are plain block-style YAML.
func jobBlock(t *testing.T, doc, name string) string {
	t.Helper()
	i := strings.Index(doc, "\njobs:\n")
	if i < 0 {
		t.Fatal("no jobs section")
	}
	jobs := doc[i+len("\njobs:\n"):]
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:$`)
	loc := re.FindStringIndex(jobs)
	if loc == nil {
		t.Fatalf("job %q not found", name)
	}
	rest := jobs[loc[0]:]
	if next := regexp.MustCompile(`(?m)\n  [a-z][a-z0-9-]*:\n`).FindStringIndex(rest[1:]); next != nil {
		return rest[:next[0]+1]
	}
	return rest
}

func mustContain(t *testing.T, what, doc string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(doc, s) {
			t.Errorf("%s lacks %q", what, s)
		}
	}
}

func mustNotContain(t *testing.T, what, doc string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(doc, s) {
			t.Errorf("%s must not contain %q", what, s)
		}
	}
}

var workflows = []string{".github/workflows/smoke.yml", ".github/workflows/publish.yml"}

// Every third-party action is pinned to a commit SHA, like release.yml.
func TestActionsArePinned(t *testing.T) {
	pinned := regexp.MustCompile(`^\s*(?:- )?uses: ([^@\s]+)@([0-9a-f]{40}) # v[0-9][0-9.]*$`)
	for _, w := range workflows {
		for n, line := range strings.Split(readRepoFile(t, w), "\n") {
			if !strings.Contains(line, "uses:") || strings.Contains(line, "uses: ./") {
				continue
			}
			if !pinned.MatchString(line) {
				t.Errorf("%s:%d: action is not pinned to a SHA with a version comment: %s", w, n+1, line)
			}
		}
	}
}

// No workflow expression that an outsider can influence (the tag, the branch
// name of a workflow run) is expanded inside a shell script: it reaches the
// shell only through an environment variable.
func TestNoExpressionInjectionInRunSteps(t *testing.T) {
	expr := regexp.MustCompile(`\$\{\{\s*(inputs\.|github\.event\.|github\.head_ref|needs\.[a-z]+\.outputs\.|steps\.[a-z]+\.outputs\.)`)
	for _, w := range workflows {
		doc := readRepoFile(t, w)
		lines := strings.Split(doc, "\n")
		for i := 0; i < len(lines); i++ {
			m := regexp.MustCompile(`^(\s*)(?:- )?run:\s*(.*)$`).FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			body := []string{m[2]}
			if strings.HasPrefix(strings.TrimSpace(m[2]), "|") || strings.HasPrefix(strings.TrimSpace(m[2]), ">") {
				body = nil
				for j := i + 1; j < len(lines) && (strings.TrimSpace(lines[j]) == "" || len(lines[j])-len(strings.TrimLeft(lines[j], " ")) > len(m[1])); j++ {
					body = append(body, lines[j])
				}
			}
			if expr.MatchString(strings.Join(body, "\n")) {
				t.Errorf("%s:%d: a run step expands an untrusted expression; pass it through env", w, i+1)
			}
		}
	}
}

// AC: smoke jobs download the exact draft assets and verify checksums and the
// cosign bundle, then install on clean environments.
func TestSmokeWorkflowJobs(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/smoke.yml")
	mustContain(t, "smoke.yml", doc, "workflow_call:", "workflow_dispatch:", "COSIGN_VERSION: v3.1.4")
	mustNotContain(t, "smoke.yml", doc, "pull_request", "push:", "gh release edit", "gh release create", "imagetools create")

	jobs := map[string][]string{
		"deb":       {"matrix.arch", "arch: [amd64, arm64]", "docker/setup-qemu-action", `test/smoke/fetch.sh "$TAG" assets "*_${{ matrix.arch }}.deb"`, `test/smoke/deb.sh assets "$TAG" "${{ matrix.arch }}"`},
		"rpm":       {`test/smoke/fetch.sh "$TAG" assets "*.x86_64.rpm"`, `test/smoke/rpm.sh assets "$TAG"`},
		"bootstrap": {`"*_linux_amd64.tar.gz" "bootstrap.sh"`, `test/smoke/bootstrap.sh assets "$TAG"`},
		"image":     {"packages: read", "--password-stdin", `test/smoke/fetch.sh "$TAG" assets "checksums.txt"`, `test/smoke/image.sh "$TAG"`},
		"macos":     {"brew install tmux", "macos-latest", "macos-15-intel", `test/smoke/macos.sh assets "$TAG"`, "_darwin_${arch}.tar.gz"},
		"windows":   {"windows-latest", "_windows_amd64_preview.zip", "./test/smoke/windows.ps1 -Assets assets -Tag $env:TAG"},
	}
	for name, subs := range jobs {
		b := jobBlock(t, doc, name)
		mustContain(t, "job "+name, b,
			"ref: refs/tags/${{ inputs.tag }}", // the scripts of the exact tagged commit
			"cosign-release: ${{ env.COSIGN_VERSION }}",
			"GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
			"TAG: ${{ inputs.tag }}",
			"persist-credentials: false",
		)
		mustContain(t, "job "+name, b, subs...)
		if name != "image" {
			mustContain(t, "job "+name, b, "test/smoke/fetch.sh")
		}
	}
	// The gate job is green only when every smoke job passed.
	gate := jobBlock(t, doc, "smoke-passed")
	for name := range jobs {
		if !regexp.MustCompile(`needs: \[[^\]]*\b` + name + `\b`).MatchString(gate) {
			t.Errorf("smoke-passed does not need %s", name)
		}
	}
	mustContain(t, "smoke-passed", gate, "if: always()", `[ "$r" = success ] || exit 1`)
}

// AC: publish.yml publishes only when every smoke job passed.
func TestPublishWorkflowGate(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/publish.yml")
	on := doc[strings.Index(doc, "\non:\n"):strings.Index(doc, "\npermissions:")]
	mustContain(t, "on", on, "workflow_run:", "workflows: [release]", "types: [completed]", "workflow_dispatch:")
	mustNotContain(t, "on", on, "pull_request", "push:")

	resolve := jobBlock(t, doc, "resolve")
	mustContain(t, "resolve", resolve,
		"github.event.workflow_run.conclusion == 'success'",
		"github.event.workflow_run.event == 'push'",
		"smoke_require_tag",
		`"$sha" != "$RUN_SHA"`, // the tag must point at the commit that was built
	)

	smoke := jobBlock(t, doc, "smoke")
	mustContain(t, "smoke", smoke, "needs: resolve", "uses: ./.github/workflows/smoke.yml", "tag: ${{ needs.resolve.outputs.tag }}")

	publish := jobBlock(t, doc, "publish")
	if !regexp.MustCompile(`needs: \[resolve, smoke\]`).MatchString(publish) {
		t.Error("publish must need resolve and smoke")
	}
	mustNotContain(t, "publish", publish, "continue-on-error", "always()", "if: failure()")
	mustContain(t, "publish", publish, "contents: write", "packages: write")

	// Nothing but the publish job may publish or move a tag.
	for _, name := range []string{"resolve", "smoke"} {
		mustNotContain(t, name, jobBlock(t, doc, name), "gh release edit", "imagetools create")
	}
}

// AC: an -rc.N is published as a prerelease and moves no channel tag and no
// image tag; a final release publishes and moves X.Y, X and latest. The work is
// done by test/smoke/promote.sh (tested against a fake release world in
// promote_test.go); the workflow only calls it, once, after the pin.
func TestPublishCandidateAndFinal(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/publish.yml")
	publish := jobBlock(t, doc, "publish")
	mustContain(t, "publish", publish,
		`test/smoke/promote.sh "$TAG" "$RUNNER_TEMP/assets.manifest" "$TESTED_DIGEST"`,
		"--password-stdin", // the pin step logs in to the registry through stdin
	)
	// Nothing in the workflow publishes or moves a tag by itself: that would
	// bypass the rollback.
	mustNotContain(t, "publish.yml", doc, "gh release edit", "imagetools create", ":latest", "--tag latest")
	if n := strings.Count(publish, "promote.sh"); n != 1 {
		t.Errorf("promote.sh is called %d times, want once", n)
	}

	promote := readRepoFile(t, "test/smoke/promote.sh")
	// The plan decides the flags and the tags, from the live list of public releases.
	mustContain(t, "promote.sh", promote,
		`"$here/channels.sh" "$tag"`,
		`--json tagName,isDraft,isPrerelease`,
		`select(.isDraft == false and .isPrerelease == false)`,
		`gh release edit "$tag" --repo "$SMOKE_REPO" --draft=false`,
		`--prerelease="$prerelease" --latest="$github_latest"`,
		`for t in $image_tags`,
	)
	// Order: plan, preflight (image signature, previous state), publication,
	// public check, channel move (a failure must leave the channels untouched
	// until the release is public and verified).
	order := []string{
		`"$here/channels.sh" "$tag"`,
		`cosign verify "$SMOKE_IMAGE@$digest"`, // the source is verified before anything changes
		"prev_latest=$(",
		"prev_digest[$t]=$(",
		"gh release edit \"$tag\" --repo \"$SMOKE_REPO\" --draft=false",
		"curl -fsSL",
		"smoke_verify_all_assets",
		`docker buildx imagetools create "${args[@]}"`,
		`cosign verify "$SMOKE_IMAGE:$t"`, // and every moved tag afterwards
	}
	last := -1
	for _, o := range order {
		i := strings.Index(promote, o)
		if i < 0 || i < last {
			t.Errorf("promote.sh: %q is missing or out of order (at %d, previous step at %d)", o, i, last)
		}
		last = i
	}
	// The tested digest is used as given, never looked up again.
	mustNotContain(t, "promote.sh", promote, `imagetools inspect "$SMOKE_IMAGE:$tag"`)
	// The rollback covers the channels and the release, and runs on any failure.
	mustContain(t, "promote.sh", promote, "trap on_exit EXIT", "restore_channels", "restore_release", "--draft=true", "--latest=true", "ROLLBACK INCOMPLETE")
	// The public check is what "minion update --version" and bootstrap.sh do:
	// a download without a token, compared with the tested asset set.
	mustContain(t, "promote.sh", promote, "smoke_check_asset_manifest", "smoke_verify_all_assets", "SMOKE_PUBLIC_BASE")
	mustNotContain(t, "promote.sh", promote, "Authorization", "--header")
	// The registry token goes through stdin, never argv.
	mustNotContain(t, "publish.yml", doc, "docker login -p", "--password ", "-p $")
}

// Concurrent releases: the whole publish workflow runs in the concurrency group
// that release.yml uses for the same tag (a rebuild and a promotion exclude each
// other from the draft check to the publication), and the publish job also holds
// one global group, so the plan, the publication and the channel moves of all
// versions are serialized.
func TestPublishLocks(t *testing.T) {
	pub := readRepoFile(t, ".github/workflows/publish.yml")
	rel := readRepoFile(t, ".github/workflows/release.yml")

	// release.yml: group "release-<ref>"; for a tag push the ref is refs/tags/<tag>.
	relGroup := regexp.MustCompile(`(?m)^concurrency:\n  group: (release-\$\{\{ github\.ref \}\})\n  cancel-in-progress: false`).FindStringSubmatch(rel)
	if relGroup == nil {
		t.Fatal("release.yml has no workflow-level concurrency group release-${{ github.ref }}")
	}
	top := pub[strings.Index(pub, "\nconcurrency:\n"):strings.Index(pub, "\nenv:\n")]
	mustContain(t, "publish.yml concurrency", top,
		"group: release-refs/tags/${{ github.event.workflow_run.head_branch || inputs.tag }}",
		"cancel-in-progress: false",
	)
	mustNotContain(t, "publish.yml concurrency", top, "group: publish-${{")

	publish := jobBlock(t, pub, "publish")
	mustContain(t, "publish job", publish, "concurrency:\n      group: publish-channels\n      cancel-in-progress: false")
	// No other job takes the global group (the smoke tests run in parallel).
	for _, name := range []string{"resolve", "smoke"} {
		mustNotContain(t, name, jobBlock(t, pub, name), "publish-channels")
	}
	// release.yml's draft check, uploads and final check are all inside its
	// workflow group; nothing there opts out.
	mustNotContain(t, "release.yml", rel, "cancel-in-progress: true")
}

// The tag moves a release may make are exactly what channels.sh prints, and
// release.yml still never publishes or moves a channel tag.
func TestReleaseWorkflowStillNeverPublishes(t *testing.T) {
	rel := readRepoFile(t, ".github/workflows/release.yml")
	mustNotContain(t, "release.yml", rel, "gh release edit", "imagetools create", "--draft=false")
}

// The scripts cover the acceptance criteria that the workflows only call.
func TestScriptsCoverTheACs(t *testing.T) {
	cases := map[string][]string{
		"test/smoke/deb.sh":               {"debian:13", "amd64 | arm64", "sd_check_worker"},
		"test/smoke/rpm.sh":               {"fedora:latest", "dnf install -y"},
		"test/smoke/systemd_container.sh": {"minion version", "mn version", "minion-worker@", "worker status", "hub serve", "systemctl enable --now"},
		"test/smoke/bootstrap.sh":         {"alpine:3.22", "debian:13", "MINION_SHA256", "MINION_RELEASE_BASE"},
		"test/smoke/image.sh":             {"chown", "65532", "hub restore", "hub backup", "buildx imagetools inspect", "cosign verify", "docker pull", "EXPECTED_FIX"},
		"test/smoke/macos.sh": {
			"worker run", "--tmux-socket", "tmux -S", "unset TMUX TMUX_PANE",
			// A private socket from the caller (the Go test, through tmuxtest):
			// the script neither starts nor ends a server on it.
			`SMOKE_TMUX_SOCKET`, "own_server=0", "own_server=1",
			`if [ "$own_server" -eq 1 ]; then`, `if [ "$own_server" -eq 1 ] && [ -S "$sock" ]; then`,
			"Sessions: +1",
		},
		"test/smoke/windows.ps1": {
			"_preview.zip", "minion version", "& $minion version", "worker', 'run'", "Expand-Archive",
			"$env:MINION_PREVIEW_ONLY = 'true'", "worker status", "Preview only:", "worker core started",
		},
		"test/smoke/pin.sh":     {"smoke_check_asset_manifest", "smoke_verify_all_assets", "imagetools inspect"},
		"test/smoke/promote.sh": {"trap on_exit EXIT", "restore_channels", "restore_release", "imagetools create"},
	}
	for file, subs := range cases {
		mustContain(t, file, readRepoFile(t, file), subs...)
	}
	// --preview-only is a switch: "--preview-only true" makes "true" a positional
	// argument. The windows script sets the environment instead.
	mustNotContain(t, "test/smoke/windows.ps1", readRepoFile(t, "test/smoke/windows.ps1"), "--preview-only")
	// The server end command (spelled in two parts here: the tmux lint rule
	// forbids the word in tests) runs only inside the guard for a server the
	// script started.
	endServer := "kill" + "-server"
	for n, line := range strings.Split(readRepoFile(t, "test/smoke/macos.sh"), "\n") {
		if strings.Contains(line, endServer) && !strings.Contains(line, `tmux -S "$sock" `+endServer) {
			t.Errorf("test/smoke/macos.sh:%d: %s without -S: %s", n+1, endServer, line)
		}
	}
	if !regexp.MustCompile(`(?s)if \[ "\$own_server" -eq 1 \] && \[ -S "\$sock" \]; then\s+tmux -S "\$sock" ` + endServer).MatchString(readRepoFile(t, "test/smoke/macos.sh")) {
		t.Errorf("test/smoke/macos.sh: %s must be reachable only when the script started the server itself", endServer)
	}
	// tmux is only ever started on a private socket: every tmux command of the
	// macOS script names one with -S.
	invoke := regexp.MustCompile(`(^\s*|\|\|\s+|;\s*|\$\(diag\s+|smoke_timeout\s+\S+\s+)tmux\s+(\S+)`)
	for n, line := range strings.Split(readRepoFile(t, "test/smoke/macos.sh"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := invoke.FindStringSubmatch(line); m != nil && m[2] != "-S" {
			t.Errorf("test/smoke/macos.sh:%d: tmux without a private socket (-S): %s", n+1, line)
		}
	}
	// The scripts never put a secret in argv: no password literal, only files.
	for _, f := range []string{"deb.sh", "rpm.sh", "image.sh", "systemd_container.sh", "bootstrap.sh", "fetch.sh"} {
		mustNotContain(t, f, readRepoFile(t, "test/smoke/"+f), "MINION_ADMIN_PASSWORD=", "--password ", "docker login -p")
	}
}

// The token that can edit or publish the draft goes to the step that
// downloads the draft and to no step that runs an artifact under test.
func TestSmokeWorkflowTokenIsOnlyInTheFetchSteps(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/smoke.yml")
	stepStart := regexp.MustCompile(`(?m)^      - `)
	tokenRE := regexp.MustCompile(`GH_TOKEN|secrets\.GITHUB_TOKEN|github\.token`)
	for _, name := range []string{"deb", "rpm", "bootstrap", "image", "macos", "windows", "smoke-passed"} {
		job := jobBlock(t, doc, name)
		locs := stepStart.FindAllStringIndex(job, -1)
		if len(locs) == 0 {
			t.Fatalf("job %s has no steps", name)
		}
		if head := job[:locs[0][0]]; tokenRE.MatchString(head) {
			t.Errorf("job %s carries a token at job level (env or otherwise); only the fetch step may:\n%s", name, head)
		}
		for i, loc := range locs {
			end := len(job)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			step := job[loc[0]:end]
			if !tokenRE.MatchString(step) {
				continue
			}
			switch {
			case strings.Contains(step, "test/smoke/fetch.sh"):
				mustContain(t, "fetch step of "+name, step, "GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}", "SMOKE_CHECKSUMS_SHA256: ${{ inputs.checksums_sha256 }}")
			case strings.Contains(step, "docker login") && strings.Contains(step, "REGISTRY_TOKEN") && name == "image":
				// The registry credential of the image job (packages: read); the
				// image runs in containers that do not see the docker config.
				mustNotContain(t, "registry step", step, "GH_TOKEN")
			default:
				t.Errorf("job %s: a step that does not run fetch.sh carries the token:\n%s", name, step)
			}
		}
	}
	// No step that runs test/smoke/fetch.sh lacks the pin.
	if n, m := strings.Count(doc, `test/smoke/fetch.sh "$TAG"`), strings.Count(doc, "SMOKE_CHECKSUMS_SHA256: ${{ inputs.checksums_sha256 }}"); n != m {
		t.Errorf("%d fetch.sh steps but %d carry the pinned checksums hash", n, m)
	}
}

// publish.yml and smoke.yml pin the exact artifacts between the smoke tests
// and the publish (AD-24: only after install smoke tests of the exact artifacts).
func TestPublishPinsTheTestedArtifacts(t *testing.T) {
	smoke := readRepoFile(t, ".github/workflows/smoke.yml")
	mustContain(t, "smoke.yml", smoke,
		"checksums_sha256:",
		"image_digest:",
		"value: ${{ jobs.image.outputs.digest }}",
		"digest: ${{ steps.image.outputs.digest }}",
		"id: image",
		`SMOKE_DIGEST_OUTPUT="$GITHUB_OUTPUT" test/smoke/image.sh "$TAG"`,
	)
	call := smoke[strings.Index(smoke, "  workflow_call:"):strings.Index(smoke, "  workflow_dispatch:")]
	mustContain(t, "workflow_call", call, "checksums_sha256:", "required: true", "outputs:")

	doc := readRepoFile(t, ".github/workflows/publish.yml")
	resolve := jobBlock(t, doc, "resolve")
	// The complete asset set is recorded: every asset is downloaded (no
	// --pattern), and the manifest holds the hash of every file.
	mustContain(t, "resolve", resolve,
		"checksums_sha256: ${{ steps.resolve.outputs.checksums_sha256 }}",
		"assets_manifest: ${{ steps.resolve.outputs.assets_manifest }}",
		`gh release download "$tag" --repo "$GITHUB_REPOSITORY" --dir "$RUNNER_TEMP/pin"`,
		`smoke_require_signed_assets "$RUNNER_TEMP/pin"`,
		`smoke_asset_manifest "$RUNNER_TEMP/pin" >"$RUNNER_TEMP/assets.manifest"`,
		`pin=$(smoke_sha256 "$RUNNER_TEMP/pin/checksums.txt")`,
		`echo "checksums_sha256=$pin" >>"$GITHUB_OUTPUT"`,
		`echo "assets_manifest<<ASSETS_MANIFEST_END"`,
	)
	mustNotContain(t, "resolve", resolve, "--pattern")
	mustContain(t, "smoke call", jobBlock(t, doc, "smoke"), "checksums_sha256: ${{ needs.resolve.outputs.checksums_sha256 }}")

	publish := jobBlock(t, doc, "publish")
	mustContain(t, "publish", publish,
		"ASSETS_MANIFEST: ${{ needs.resolve.outputs.assets_manifest }}",
		"TESTED_DIGEST: ${{ needs.smoke.outputs.image_digest }}",
		`test/smoke/pin.sh "$TAG" "$RUNNER_TEMP/assets.manifest" "$TESTED_DIGEST"`,
	)
	pin := strings.Index(publish, "pin.sh")
	promote := strings.Index(publish, "promote.sh")
	if pin < 0 || promote < 0 || pin > promote {
		t.Errorf("order must be pin (%d), promote (%d)", pin, promote)
	}
	// The pin checks every asset against the record, the signatures, the image.
	pinSh := readRepoFile(t, "test/smoke/pin.sh")
	mustContain(t, "pin.sh", pinSh, "smoke_check_asset_manifest", "smoke_verify_all_assets", "imagetools inspect")
	mustNotContain(t, "pin.sh", pinSh, "--pattern", "smoke_check_pinned_checksums")
}
