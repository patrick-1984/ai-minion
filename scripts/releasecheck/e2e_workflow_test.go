package releasecheck

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Story 9.6: the container end-to-end job is a gate. e2e.yml runs on the right
// pull requests and on every tag; release.yml repeats its jobs on the tagged
// commit and its release job needs them, so goreleaser cannot run (and no
// draft, image push or signature happens) when an end-to-end job failed. These
// tests read the workflow files as text, like the other tests of this package,
// and check what the jobs run and what gates what, not how it is spelled.

func TestE2EWorkflowTriggers(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/e2e.yml")
	on := section(t, doc, "on")
	mustContain(t, "on", on, "pull_request:", "push:", "tags:", `- "**"`) // every tag
	mustNotContain(t, "on", on, "branches:")
	for _, p := range []string{"internal/hub/**", "internal/worker/**", "internal/transport/**", "cmd/**", "test/e2e/**",
		"Makefile", ".goreleaser.yaml", ".ko.yaml", "deploy/**", "go.mod", ".github/workflows/e2e.yml",
		".github/workflows/release.yml", "scripts/releasecheck/**"} {
		mustContain(t, "pull_request paths", on, "- "+p+"\n")
	}
	perms := section(t, doc, "permissions")
	mustContain(t, "permissions", perms, "contents: read")
	mustNotContain(t, "permissions", perms, "write")
}

// release.yml runs the end-to-end jobs on tags only, so a pull request that
// changes the release pipeline would reach a tag without having run them. Every
// path that starts release.yml's own pull request run must therefore also
// start e2e.yml's; a path added to release.yml later fails here until e2e.yml
// follows.
func TestE2EWorkflowRunsOnEveryPullRequestPathOfTheReleaseWorkflow(t *testing.T) {
	e2ePaths := pullRequestPaths(t, ".github/workflows/e2e.yml")
	releasePaths := pullRequestPaths(t, ".github/workflows/release.yml")
	if len(releasePaths) < 5 || len(e2ePaths) < 10 {
		t.Fatalf("parsed %d release and %d e2e pull_request paths, the parser is broken", len(releasePaths), len(e2ePaths))
	}
	for _, p := range releasePaths {
		if !slices.Contains(e2ePaths, p) {
			t.Errorf("release.yml runs on pull requests that change %s but e2e.yml does not; its tag-only end-to-end jobs would be skipped", p)
		}
	}
	for _, p := range []string{".github/workflows/release.yml", "scripts/releasecheck/**"} {
		if !slices.Contains(releasePaths, p) || !slices.Contains(e2ePaths, p) {
			t.Errorf("%s must trigger both workflows", p)
		}
	}
}

// pullRequestPaths returns the "paths" list of the pull_request trigger of a
// workflow file, read as text like the rest of this package.
func pullRequestPaths(t *testing.T, file string) []string {
	t.Helper()
	on := section(t, readRepoFile(t, file), "on")
	_, after, ok := strings.Cut(on, "pull_request:")
	if !ok {
		t.Fatalf("%s has no pull_request trigger", file)
	}
	var out []string
	inPaths := false
	for _, line := range strings.Split(after, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case trim == "paths:":
			inPaths = true
		case !inPaths || trim == "" || strings.HasPrefix(trim, "#"):
		case strings.HasPrefix(trim, "- "):
			out = append(out, strings.Trim(strings.TrimPrefix(trim, "- "), `"'`))
		default: // the next key (push:, ...) ends the list
			return out
		}
	}
	return out
}

func TestE2EWorkflowJobsRunTheRealCommands(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/e2e.yml")
	inproc := jobBlock(t, doc, "in-process")
	mustContain(t, "in-process job", inproc, "run: make e2e")
	container := jobBlock(t, doc, "container")
	mustContain(t, "container job", container,
		"ko-build/setup-ko@", "version: ${{ env.KO_VERSION }}",
		"go vet -tags dockere2e ./test/e2e/docker/...",
		"run: test/e2e/docker/run.sh all")
	mustContain(t, "workflow env", section(t, doc, "env"), "KO_VERSION: v0.19.1", `MINION_REQUIRE_TMUX: "1"`)
	// The same pinned versions as the release workflow (nothing new is pulled in).
	rel := readRepoFile(t, ".github/workflows/release.yml")
	if !strings.Contains(section(t, rel, "env"), "KO_VERSION: v0.19.1") {
		t.Error("release.yml and e2e.yml must use the same ko version")
	}
	// The Makefile target is the race run the story names.
	mk := readRepoFile(t, "Makefile")
	if !regexp.MustCompile(`(?m)^e2e:.*\n\tgo test -race -tags e2e `).MatchString(mk) {
		t.Error("make e2e must run go test -race -tags e2e")
	}
}

func TestE2EWorkflowIsPinnedAndHasNoSecretsOrWrites(t *testing.T) {
	doc := readRepoFile(t, ".github/workflows/e2e.yml")
	pinned := regexp.MustCompile(`^[\w./-]+@[0-9a-f]{40}( #.*)?$`)
	n := 0
	for _, m := range regexp.MustCompile(`(?m)^\s*(?:- )?uses: (.*)$`).FindAllStringSubmatch(doc, -1) {
		n++
		if !pinned.MatchString(strings.TrimSpace(m[1])) {
			t.Errorf("action %q is not pinned to a commit SHA", m[1])
		}
	}
	if n == 0 {
		t.Fatal("found no actions: the parser is broken")
	}
	mustNotContain(t, "e2e.yml", doc, "secrets.", "id-token", "packages: write", "contents: write", "docker login", "docker push", "--push")
}

// Every command e2e.yml runs is run, verbatim, by the tag-only copies in
// release.yml, and the release job needs those copies.
func TestReleaseWorkflowGatesGoreleaserOnTheE2EJobs(t *testing.T) {
	e2e := readRepoFile(t, ".github/workflows/e2e.yml")
	rel := readRepoFile(t, ".github/workflows/release.yml")

	relCount := map[string]int{}
	for _, s := range runSteps(rel) {
		relCount[s]++
	}
	steps := runSteps(e2e)
	if len(steps) < 5 {
		t.Fatalf("parsed only %d run steps from e2e.yml, the parser is broken", len(steps))
	}
	want := map[string]int{}
	for _, s := range steps {
		want[s]++
	}
	for step, n := range want {
		if relCount[step] < n {
			t.Errorf("release.yml runs this e2e.yml step %d time(s), want at least %d:\n%s", relCount[step], n, step)
		}
	}

	release := jobBlock(t, rel, "release")
	needs := regexp.MustCompile(`needs:\n((?:      - .*\n)+)`).FindStringSubmatch(release)
	if needs == nil {
		t.Fatal("the release job has no needs list")
	}
	for _, j := range []string{"tests-e2e", "e2e-docker"} {
		if !strings.Contains(needs[1], "- "+j+"\n") {
			t.Errorf("the release job must need %s", j)
		}
		mustContain(t, j, jobBlock(t, rel, j), "if: github.ref_type == 'tag'")
	}
	// Both jobs build and test the commit that the tag points at: they check it
	// out themselves (no ref override) and never see publishing permissions.
	for _, j := range []string{"tests-e2e", "e2e-docker"} {
		b := jobBlock(t, rel, j)
		mustNotContain(t, j, b, "permissions:", "secrets.", "ref:")
	}
}

// The compose file is the contract of the container job: what the Hub runs as,
// what it publishes, how it learns about the proxy.
func TestE2EComposeFile(t *testing.T) {
	c := readRepoFile(t, "test/e2e/docker/compose.yaml")
	mustContain(t, "compose.yaml", c,
		`user: "65532:65532"`,                  // the non-root user of the image (A-14)
		"command: [\"chown\", \"65532:65532\"", // the documented data directory preparation
		"MINION_ADMIN_PASSWORD_FILE: /run/secrets/admin_password",
		"file: ${E2E_ADMIN_PASSWORD_FILE:?}", // the admin secret is a file
		`MINION_TLS: "off"`,
		"MINION_PUBLIC_URL: ${E2E_PUBLIC_URL:?}",
		"MINION_TRUSTED_PROXIES: 10.89.77.10/32", // the proxy's address only, not the network
		"ipv4_address: 10.89.77.10",
		"cap_drop: [ALL]", "no-new-privileges:true",
	)
	mustNotContain(t, "compose.yaml", c, "privileged", "network_mode: host", "docker.sock", "MINION_ADMIN_PASSWORD:")
	// Every published port is a loopback port.
	ports := regexp.MustCompile(`(?m)^\s+- "([^"]*:[^"]*)"\s*$`).FindAllStringSubmatch(c, -1)
	if len(ports) < 2 {
		t.Fatalf("found %d port mappings, want the Hub's and the proxy's", len(ports))
	}
	for _, p := range ports {
		if !strings.HasPrefix(p[1], "127.0.0.1:") {
			t.Errorf("port %q is not published on loopback only", p[1])
		}
	}
	// The Hub of the proxy variant publishes nothing: only the proxy is reachable.
	hubProxied := regexp.MustCompile(`(?s)\n  hub-proxied:\n(.*?)\n  proxy:`).FindStringSubmatch(c)
	if hubProxied == nil {
		t.Fatal("hub-proxied service not found")
	}
	mustNotContain(t, "hub-proxied", hubProxied[1], "ports:")
}

// run.sh is checked by what it runs, with the same shellcheck the other
// scripts of the repository get, and by the safety rules of the story.
func TestE2ERunScript(t *testing.T) {
	s := readRepoFile(t, "test/e2e/docker/run.sh")
	mustContain(t, "run.sh", s,
		"unset TMUX TMUX_PANE",
		"ko build ./cmd/minion --local --bare",                 // the exact source image through the existing ko config
		"go test -tags dockere2e",                              // the checks of the job
		"docker compose -p", "down --volumes --remove-orphans", // its own project, cleaned by name
		"default tmux socket", // the final assertion
		`[ ! -e "$default_sock" ]`,
	)
	// The script never runs a tmux command (the checks use tmuxtest), never
	// stops a server, never uses a blanket cleanup of the host.
	for _, bad := range []string{"kill-server", "tmux -", "docker system prune", "docker rm -f $(", "pkill", "killall", "--insecure", "-k ", "InsecureSkipVerify"} {
		mustNotContain(t, "run.sh", s, bad)
	}
	if res := run(t, repoRoot(t), nil, "bash", "-n", "test/e2e/docker/run.sh"); res.code != 0 {
		t.Errorf("bash -n run.sh:\n%s", res.combined())
	}
	needTools(t, "shellcheck")
	if res := run(t, repoRoot(t), nil, "shellcheck", "test/e2e/docker/run.sh"); res.code != 0 {
		t.Errorf("shellcheck run.sh:\n%s", res.combined())
	}
}

// The Go checks verify TLS for real: they trust one certificate and one host
// name and refuse a wrong one; nothing in them skips verification.
func TestE2EChecksDoNotDisableTLSVerification(t *testing.T) {
	src := readRepoFile(t, "test/e2e/docker/checks_test.go")
	mustNotContain(t, "checks_test.go", src, "InsecureSkipVerify", "--insecure", "hub-insecure", "exec.Command(\"tmux\"")
	mustContain(t, "checks_test.go", src, "RootCAs: pool", "ServerName: serverName",
		"not-the-hub.invalid",     // the wrong-name client must fail
		"tmuxtest.New(t)",         // the private tmux server
		"E2E_DEFAULT_TMUX_SOCKET") // the default socket assertion
}
