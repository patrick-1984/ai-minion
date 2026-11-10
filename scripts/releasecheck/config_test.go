package releasecheck

import (
	"regexp"
	"strings"
	"testing"
)

const versionPkg = "github.com/patrick-1984/ai-minion/internal/version"

// AC "goreleaser builds linux amd64/arm64/armv7 (GOARM=7), darwin amd64/arm64,
// windows amd64/arm64 with CGO_ENABLED=0 and version ldflags".
func TestGoreleaserBuilds(t *testing.T) {
	doc := readRepoFile(t, ".goreleaser.yaml")
	if !regexp.MustCompile(`(?m)^version: 2$`).MatchString(doc) {
		t.Error("the configuration must declare version: 2")
	}
	b := section(t, doc, "builds")
	mustContain(t, "builds", b,
		"main: ./cmd/minion",
		"- CGO_ENABLED=0",
		"-trimpath",
		"goos: [linux, darwin, windows]",
		"goarch: [amd64, arm64, arm]",
		`goarm: ["7"]`,
		// armv7 exists for linux only.
		"{ goos: darwin, goarch: arm }",
		"{ goos: windows, goarch: arm }",
		"-X "+versionPkg+".Version=v{{ .Version }}",
		"-X "+versionPkg+".Commit={{ .ShortCommit }}",
		"-X "+versionPkg+".Date={{ .CommitDate }}",
	)
	mustNotContain(t, "builds", b, "CGO_ENABLED=1")
}

// AC "archives are tar.gz (zip for Windows, named with preview)".
func TestGoreleaserArchives(t *testing.T) {
	a := section(t, readRepoFile(t, ".goreleaser.yaml"), "archives")
	mustContain(t, "archives", a,
		"formats: [tar.gz]",
		"- goos: windows",
		"formats: [zip]",
		`{{ if eq .Os "windows" }}_preview{{ end }}`,
		"{{ with .Arm }}v{{ . }}{{ end }}", // linux_armv7
	)
}

// AC "nfpm builds deb and rpm with /usr/bin/minion, an mn symlink,
// /usr/lib/systemd/system/minion-worker@.service (from deploy/systemd), a tmux
// dependency".
func TestGoreleaserPackages(t *testing.T) {
	n := section(t, readRepoFile(t, ".goreleaser.yaml"), "nfpms")
	mustContain(t, "nfpms", n,
		"formats: [deb, rpm]",
		"bindir: /usr/bin",
		"package_name: minion",
		"license: Apache-2.0",
		"dependencies:\n      - tmux",
		"- src: minion\n        dst: /usr/bin/mn\n        type: symlink",
		"- src: deploy/systemd/minion-worker@.service\n        dst: /usr/lib/systemd/system/minion-worker@.service",
		"postinstall: deploy/nfpm/postinstall.sh",
		"preremove: deploy/nfpm/preremove.sh",
	)
	for _, f := range []string{"deploy/systemd/minion-worker@.service", "deploy/nfpm/postinstall.sh", "deploy/nfpm/preremove.sh"} {
		readRepoFile(t, f) // the referenced files exist
	}
}

// AC "checksums.txt, an SBOM per archive ... are attached" and "checksums.txt is
// signed with cosign keyless producing a .sigstore.json bundle".
func TestGoreleaserChecksumsSbomAndSignature(t *testing.T) {
	doc := readRepoFile(t, ".goreleaser.yaml")
	mustContain(t, "checksum", section(t, doc, "checksum"),
		"name_template: checksums.txt", "algorithm: sha256")
	mustContain(t, "sboms", section(t, doc, "sboms"),
		"artifacts: archive", `"${artifact}.sbom.json"`, "cmd: syft")
	s := section(t, doc, "signs")
	mustContain(t, "signs", s,
		"artifacts: checksum",
		`signature: "${artifact}.sigstore.json"`,
		"cosign sign-blob --yes --bundle=${signature} ${artifact}",
		// snapshot builds must not start a keyless (browser) flow
		`if [ "{{ .IsSnapshot }}" = "true" ]`,
	)
	mustNotContain(t, "signs", s, "--key", "COSIGN_PASSWORD", "--yes=false")
}

// AC "ko ... ghcr.io/patrick-1984/minion for linux/amd64, linux/arm64 and
// linux/arm/v7 on gcr.io/distroless/static-debian12:nonroot, tagged vX.Y.Z only,
// cosign-signed".
func TestGoreleaserImage(t *testing.T) {
	doc := readRepoFile(t, ".goreleaser.yaml")
	k := section(t, doc, "kos")
	mustContain(t, "kos", k,
		"- ghcr.io/patrick-1984/minion",
		"base_image: gcr.io/distroless/static-debian12:nonroot",
		"- linux/amd64", "- linux/arm64", "- linux/arm/v7",
		"bare: true",
		"sbom: none", // an SBOM would add extra tags to the repository
	)
	tags := regexp.MustCompile(`\n    tags:\n((?:      - .*\n)+)`).FindStringSubmatch(k)
	if tags == nil {
		t.Fatal("kos has no tags list")
	}
	if got := strings.TrimSpace(tags[1]); got != `- "{{ .Tag }}"` {
		t.Errorf("the image is tagged with the exact version only, got %q", got)
	}
	mustNotContain(t, "kos", k, "latest", ".Major", ".Minor")

	d := section(t, doc, "docker_signs")
	mustContain(t, "docker_signs", d, "cmd: cosign", "artifacts: manifests", "- sign", "- --yes", "- ${artifact}@${digest}")
}

// AC "the GitHub release is a draft; channel tags are not moved here".
func TestGoreleaserReleaseIsDraft(t *testing.T) {
	doc := readRepoFile(t, ".goreleaser.yaml")
	r := section(t, doc, "release")
	mustContain(t, "release", r, "draft: true", "owner: patrick-1984", "name: ai-minion")
	mustNotContain(t, "release", r, "draft: false", "disable: true")
	for _, bad := range []string{"mode: replace", "tag_prefix", "homebrew", "scoop", "winget"} {
		mustNotContain(t, ".goreleaser.yaml", doc, bad)
	}
}

// The ko settings used by scripts/assert-image-platforms.sh equal those of the
// release (the assertion must check the image that is released).
func TestKoConfigMatchesGoreleaser(t *testing.T) {
	ko := readRepoFile(t, ".ko.yaml")
	gr := readRepoFile(t, ".goreleaser.yaml")
	base := "gcr.io/distroless/static-debian12:nonroot"
	mustContain(t, ".ko.yaml", ko, "defaultBaseImage: "+base,
		"- linux/amd64", "- linux/arm64", "- linux/arm/v7",
		"- CGO_ENABLED=0", "-trimpath", "main: ./cmd/minion",
		"-X "+versionPkg+".Version=", "-X "+versionPkg+".Commit=", "-X "+versionPkg+".Date=")
	mustContain(t, ".goreleaser.yaml", gr, "base_image: "+base)
}

// The systemd unit is created and owned by this story (A-6); Story 4.11
// validates the unit it generates against this file.
func TestSystemdUnit(t *testing.T) {
	u := readRepoFile(t, "deploy/systemd/minion-worker@.service")
	mustContain(t, "unit", u,
		"[Unit]", "[Service]", "[Install]",
		"User=%i",
		"ExecStart=/usr/bin/minion worker run",
		"Restart=always",
		"KillMode=process", // a restart must not end the tmux sessions
		"WantedBy=multi-user.target",
	)
}

// "goreleaser check" validates the whole configuration when goreleaser is
// installed (CI runs it as its own step as well).
func TestGoreleaserCheck(t *testing.T) {
	needTools(t, "goreleaser")
	res := run(t, repoRoot(t), nil, "goreleaser", "check")
	if res.code != 0 {
		t.Fatalf("goreleaser check failed:\n%s", res.combined())
	}
}
