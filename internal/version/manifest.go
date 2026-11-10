package version

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// manifestSchema is the only release-manifest.json schema this updater reads.
const manifestSchema = 1

// Manifest is release-manifest.json (Story 9.1): the release version, the
// protocol range of the release and the digest of every artifact. It is
// signed like checksums.txt.
type Manifest struct {
	Schema        int                `json:"schema"`
	Version       string             `json:"version"`
	ProtocolRange manifestRange      `json:"protocol_range"`
	Artifacts     []ManifestArtifact `json:"artifacts"`
}

type manifestRange struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

// ManifestArtifact is one artifact digest of the manifest.
type ManifestArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// maxManifestSize bounds the manifest and checksums downloads; both are a few
// kilobytes.
const maxManifestSize = 1 << 20

// ParseManifest parses and validates a release manifest. wantTag is the
// release tag it must describe.
func ParseManifest(data []byte, wantTag string) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("release manifest: %w", err)
	}
	if m.Schema != manifestSchema {
		return Manifest{}, fmt.Errorf("release manifest: unsupported schema %d (this minion reads schema %d); update minion by hand", m.Schema, manifestSchema)
	}
	if m.Version != wantTag {
		return Manifest{}, fmt.Errorf("release manifest describes %q, not the release %q", m.Version, wantTag)
	}
	if _, err := m.Range(); err != nil {
		return Manifest{}, fmt.Errorf("release manifest: %w", err)
	}
	for _, a := range m.Artifacts {
		if err := validArtifact(a.Name, a.SHA256); err != nil {
			return Manifest{}, fmt.Errorf("release manifest: %w", err)
		}
	}
	return m, nil
}

// Range returns the protocol range the release supports.
func (m Manifest) Range() (ProtoRange, error) {
	min, err := ParseProtoVersion(m.ProtocolRange.Min)
	if err != nil {
		return ProtoRange{}, err
	}
	max, err := ParseProtoVersion(m.ProtocolRange.Max)
	if err != nil {
		return ProtoRange{}, err
	}
	r := ProtoRange{Min: min, Max: max}
	return r, r.Validate()
}

// Digest returns the lower-case hex SHA-256 recorded for an artifact.
func (m Manifest) Digest(name string) (string, bool) {
	for _, a := range m.Artifacts {
		if a.Name == name {
			return a.SHA256, true
		}
	}
	return "", false
}

func validArtifact(name, sha string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("artifact name %q is not a plain file name", name)
	}
	if b, err := hex.DecodeString(sha); err != nil || len(b) != 32 || sha != strings.ToLower(sha) {
		return fmt.Errorf("artifact %q has an invalid sha256", name)
	}
	return nil
}

// Checksums maps an artifact name to its lower-case hex SHA-256, as listed in
// checksums.txt.
type Checksums map[string]string

// ParseChecksums parses checksums.txt: lines "<sha256>  <name>" (two spaces,
// or a space and "*"). A malformed line, or two different digests for one
// name, is an error, so a tampered list cannot be half-read.
func ParseChecksums(data []byte) (Checksums, error) {
	out := Checksums{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 67 || (line[64] != ' ') || (line[65] != ' ' && line[65] != '*') {
			return nil, fmt.Errorf("checksums.txt line %d is malformed", n)
		}
		sha, name := line[:64], line[66:]
		if err := validArtifact(name, sha); err != nil {
			return nil, fmt.Errorf("checksums.txt line %d: %w", n, err)
		}
		if prev, dup := out[name]; dup && prev != sha {
			return nil, fmt.Errorf("checksums.txt lists %q twice with different digests", name)
		}
		out[name] = sha
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("checksums.txt: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("checksums.txt is empty")
	}
	return out, nil
}

// ArchiveName returns the name of the release archive for a platform, as
// goreleaser names it (Story 9.1): minion_<version>_<os>_<arch>[v<arm>], with
// "_preview" on Windows, as .tar.gz or (Windows) .zip. version is the release
// tag with or without the leading "v".
func ArchiveName(tag, goos, goarch, goarm string) string {
	arch := goarch
	if goarch == "arm" && goarm != "" {
		arch += "v" + goarm
	}
	name := fmt.Sprintf("minion_%s_%s_%s", strings.TrimPrefix(tag, "v"), goos, arch)
	if goos == "windows" {
		return name + "_preview.zip"
	}
	return name + ".tar.gz"
}
