package releasecheck

import (
	"regexp"
	"strings"
	"testing"
)

// jobBlock returns the text of one job of the workflow ("  name:" up to the
// next job).
func jobBlock(t *testing.T, doc, name string) string {
	t.Helper()
	jobs := section(t, doc, "jobs")
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:$`)
	loc := re.FindStringIndex(jobs)
	if loc == nil {
		t.Fatalf("job %q not found", name)
	}
	rest := jobs[loc[0]:]
	next := regexp.MustCompile(`(?m)\n  [a-z][a-z0-9-]*:\n`).FindStringIndex(rest[1:])
	if next == nil {
		return rest
	}
	return rest[:next[0]+1]
}

// AC "a vX.Y.Z or vX.Y.Z-rc.N tag ... the release workflow first runs the full
// CI test suite on that exact commit and only then runs goreleaser v2.20.0".
func TestReleaseWorkflowTriggersAndGate(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/release.yml")
	on := section(t, doc, "on")
	mustContain(t, "on", on,
		`"v[0-9]+.[0-9]+.[0-9]+"`,
		`"v[0-9]+.[0-9]+.[0-9]+-rc.[0-9]+"`,
		"pull_request:", ".goreleaser.yaml", ".ko.yaml", "deploy/**",
		"scripts/assert-image-platforms.sh", "scripts/release-manifest.sh",
	)
	mustNotContain(t, "on", on, "branches:", "workflow_dispatch")

	release := jobBlock(t, doc, "release")
	mustContain(t, "release job", release, "if: github.ref_type == 'tag'")
	needs := regexp.MustCompile(`needs:\n((?:      - .*\n)+)`).FindStringSubmatch(release)
	if needs == nil {
		t.Fatal("the release job has no needs list")
	}
	for _, j := range []string{"snapshot", "tests-lint", "tests-distro-tmux", "tests-tmux-3-8", "tests-vuln", "tests-cross"} {
		if !strings.Contains(needs[1], "- "+j+"\n") {
			t.Errorf("the release job must need %s", j)
		}
		jobBlock(t, doc, j) // the job exists
	}
	for _, j := range []string{"tests-lint", "tests-distro-tmux", "tests-tmux-3-8", "tests-vuln", "tests-cross"} {
		mustContain(t, j, jobBlock(t, doc, j), "if: github.ref_type == 'tag'")
	}

	mustContain(t, "workflow env", section(t, doc, "env"),
		"GORELEASER_VERSION: v2.20.0", "KO_VERSION: v0.19.1", "COSIGN_VERSION: v3.1.4")
	mustContain(t, "release job", release,
		"version: ${{ env.GORELEASER_VERSION }}",
		"args: release --clean",
		"cosign-release: ${{ env.COSIGN_VERSION }}",
		"id-token: write", "packages: write", "contents: write",
	)
	mustNotContain(t, "release job", release, "--skip", "--snapshot")
	// goreleaser must come after the tests only through needs, and signatures
	// are checked after it.
	if i, j := strings.Index(release, "goreleaser release (draft)"), strings.Index(release, "Verify the signatures"); i < 0 || j < i {
		t.Error("the signature verification must follow the goreleaser step")
	}
}

// runSteps returns every "run:" step of a workflow as its command text: the
// value of a single-line "run: cmd", or the dedented lines of a "run: |"
// block. The workflow files are read as text on purpose (the project has no
// YAML library), which is enough because the steps are plain block scalars.
func runSteps(doc string) []string {
	var steps []string
	lines := strings.Split(doc, "\n")
	runRe := regexp.MustCompile(`^(\s*)(?:- )?run: (.*)$`)
	for i := 0; i < len(lines); i++ {
		m := runRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if m[2] != "|" {
			steps = append(steps, strings.TrimSpace(m[2]))
			continue
		}
		// The block is every following line indented deeper than the key (blank
		// lines included); its indent is the one of its first line.
		keyIndent := len(m[1])
		if strings.HasPrefix(strings.TrimLeft(lines[i], " "), "- ") {
			keyIndent += 2
		}
		var block []string
		blockIndent := -1
		for i+1 < len(lines) {
			l := lines[i+1]
			indent := len(l) - len(strings.TrimLeft(l, " "))
			if strings.TrimSpace(l) != "" && indent <= keyIndent {
				break
			}
			if blockIndent < 0 && strings.TrimSpace(l) != "" {
				blockIndent = indent
			}
			block = append(block, l)
			i++
		}
		for j, l := range block {
			if len(l) >= blockIndent && blockIndent > 0 {
				block[j] = l[blockIndent:]
			}
		}
		steps = append(steps, strings.TrimSpace(strings.Join(block, "\n")))
	}
	return steps
}

// topEnv returns the entries ("KEY: value") of the top-level env: block.
func topEnv(t *testing.T, doc string) []string {
	t.Helper()
	var entries []string
	for _, l := range strings.Split(section(t, doc, "env"), "\n")[1:] {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			entries = append(entries, l)
		}
	}
	return entries
}

// The tag test jobs run the same steps as ci.yml: every "run:" step and every
// top-level env entry of ci.yml appears verbatim in release.yml (and as often
// as in ci.yml), so a step added to ci.yml later fails this test until
// release.yml repeats it.
func TestReleaseWorkflowRunsTheCISuite(t *testing.T) {
	rel := readRepoFile(t, ".github/workflows/release.yml")
	ci := readRepoFile(t, ".github/workflows/ci.yml")

	ciSteps := runSteps(ci)
	if len(ciSteps) < 10 {
		t.Fatalf("parsed only %d run steps from ci.yml, the parser is broken", len(ciSteps))
	}
	relCount := map[string]int{}
	for _, s := range runSteps(rel) {
		relCount[s]++
	}
	ciCount := map[string]int{}
	for _, s := range ciSteps {
		ciCount[s]++
	}
	for step, n := range ciCount {
		if relCount[step] < n {
			t.Errorf("release.yml runs this ci.yml step %d time(s), want at least %d:\n%s", relCount[step], n, step)
		}
	}

	ciEnv := topEnv(t, ci)
	if len(ciEnv) == 0 {
		t.Fatal("ci.yml has no top-level env entries, the parser is broken")
	}
	relEnv := strings.Join(topEnv(t, rel), "\n")
	for _, e := range ciEnv {
		if !strings.Contains("\n"+relEnv+"\n", "\n"+e+"\n") {
			t.Errorf("release.yml lacks the ci.yml env entry %q", e)
		}
	}
}

// The parser behind TestReleaseWorkflowRunsTheCISuite sees single-line and
// block steps and the entries of the top-level env.
func TestRunStepsParser(t *testing.T) {
	doc := "env:\n  A: b\n  # c\n  D: \"e\"\njobs:\n  j:\n    steps:\n      - name: x\n        run: one\n" +
		"      - name: y\n        run: |\n          set -e\n\n          two\n            indented\n      - name: z\n        run: three\n"
	got := runSteps(doc)
	want := []string{"one", "set -e\n\ntwo\n  indented", "three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("runSteps = %q, want %q", got, want)
	}
	if env := topEnv(t, doc); strings.Join(env, "|") != `A: b|D: "e"` {
		t.Errorf("topEnv = %q", env)
	}
}

// Supply chain: every action is pinned to a commit SHA, and the workflow does
// not publish the draft or move channel tags (Story 9.7 does).
func TestReleaseWorkflowIsPinnedAndNeverPublishes(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/release.yml")
	pinned := regexp.MustCompile(`^[\w./-]+@[0-9a-f]{40}( #.*)?$`)
	for _, m := range regexp.MustCompile(`(?m)^\s*(?:- )?uses: (.*)$`).FindAllStringSubmatch(doc, -1) {
		if !pinned.MatchString(strings.TrimSpace(m[1])) {
			t.Errorf("action %q is not pinned to a commit SHA", m[1])
		}
	}
	mustNotContain(t, "release.yml", doc,
		"--draft=false", "--prerelease", "--latest", "imagetools create", "docker tag ", "docker push",
		"password ${{", "--password ", "-p ${{",
	)
	// Secrets reach commands through env or stdin only.
	if strings.Contains(doc, "docker login") && !strings.Contains(doc, "--password-stdin") {
		t.Error("docker login must read the token from stdin")
	}
	mustContain(t, "release.yml", doc, "gh api", "select(.tag_name", "grep -Fx true") // still a draft
}

// AC "bootstrap.sh (from Story 9.3 once merged) and release-manifest.json are
// attached" and the verification of the signatures.
func TestReleaseWorkflowAttachesManifestAndBootstrap(t *testing.T) {
	release := jobBlock(t, readRepoFile(t, ".github/workflows/release.yml"), "release")
	mustContain(t, "release job", release,
		"if [ -f deploy/bootstrap.sh ]",
		"scripts/release-manifest.sh --version \"$TAG\"",
		"--protocol-min \"$MINION_PROTOCOL_MIN\"",
		"--checksums dist/checksums.txt",
		"cosign sign-blob --yes --bundle \"$f.sigstore.json\" \"$f\"",
		"gh release upload \"$TAG\"",
		"cosign verify-blob",
		"--bundle dist/checksums.txt.sigstore.json",
		"--certificate-identity \"$SIGNER_IDENTITY\"",
		"https://token.actions.githubusercontent.com",
		"cosign verify \"ghcr.io/patrick-1984/minion:${TAG}\"",
		"docker buildx imagetools inspect",
		`"arm/v7"`,
	)
	mustContain(t, "release job", release,
		"SIGNER_IDENTITY: https://github.com/${{ github.repository }}/.github/workflows/release.yml@${{ github.ref }}")
}

// AC "make snapshot ... native snapshot smoke tests ... stay separate from the
// multi-platform image assertion".
func TestSnapshotJob(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/release.yml")
	s := jobBlock(t, doc, "snapshot")
	mustNotContain(t, "snapshot job", s, "if: github.ref_type", "packages: write", "id-token: write", "contents: write")
	mustContain(t, "snapshot job", s,
		"run: goreleaser check",
		"run: make snapshot",
		"version: ${{ env.KO_VERSION }}",
		"run: scripts/assert-image-platforms.sh",
		`grep -E '^version: v.+'`,
		"/usr/lib/systemd/system/minion-worker@.service",
		"dpkg-deb -c", "rpm -qlp", "grep -w tmux",
		"diff - deploy/systemd/minion-worker@.service",
	)
	// The native smoke test and the image assertion are separate steps.
	smoke := strings.Index(s, "Native snapshot smoke tests")
	assertImg := strings.Index(s, "Assert the image platforms")
	if smoke < 0 || assertImg < smoke {
		t.Error("the image assertion must be its own step after the native smoke tests")
	}
	nativeStep := s[smoke:assertImg]
	if strings.Contains(nativeStep, "assert-image-platforms") || strings.Contains(nativeStep, "docker ") {
		t.Error("the native smoke step must not do the image assertion")
	}
	// The Makefile target runs the same goreleaser configuration.
	mk := readRepoFile(t, "Makefile")
	mustContain(t, "Makefile", mk, "release --snapshot --clean", ".goreleaser.yaml")
}

// goreleaser installs before the configuration tests: TestGoreleaserCheck, the
// only test that executes goreleaser, skips itself when it is missing.
func TestSnapshotInstallsGoreleaserBeforeTheTests(t *testing.T) {
	s := jobBlock(t, readRepoFile(t, ".github/workflows/release.yml"), "snapshot")
	install := strings.Index(s, "Install goreleaser ${{ env.GORELEASER_VERSION }}")
	tests := strings.Index(s, "run: go test -race -count=1 ./scripts/releasecheck/...")
	if install < 0 || tests < 0 || install > tests {
		t.Errorf("goreleaser must be installed (at %d) before the release tests run (at %d)", install, tests)
	}
}

// The release job builds exactly the tag that triggered it, and refuses to
// rebuild a release that is already public before anything is pushed.
func TestReleaseJobBindsTheTagAndRejectsPublishedReleases(t *testing.T) {
	release := jobBlock(t, readRepoFile(t, ".github/workflows/release.yml"), "release")
	run := strings.Index(release, "goreleaser release (draft)")
	check := strings.Index(release, "The release must be absent or still a draft (before anything is pushed)")
	if check < 0 || run < 0 || check > run {
		t.Fatalf("the published-release check (at %d) must come before goreleaser (at %d)", check, run)
	}
	pre := release[check:run]
	mustContain(t, "published-release check", pre,
		"set -euo pipefail", `select(.tag_name == \"${TAG}\") | .draft`, "grep -qFxv true", "exit 1")
	// The goreleaser step is bound to the triggering tag: with -rc.N and the
	// final tag on one commit goreleaser would otherwise pick the -rc.N tag.
	step := release[run : run+strings.Index(release[run:], "Verify the signatures")]
	mustContain(t, "goreleaser step", step, "GORELEASER_CURRENT_TAG: ${{ github.ref_name }}")
}
