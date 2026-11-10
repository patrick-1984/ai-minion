package releasecheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	mediaIndex    = "application/vnd.oci.image.index.v1+json"
	mediaManifest = "application/vnd.oci.image.manifest.v1+json"
)

// layoutBuilder writes a minimal OCI image layout: blobs/sha256/<hex> plus
// index.json. It stands in for the layout that ko --oci-layout-path writes.
type layoutBuilder struct {
	t   *testing.T
	dir string
}

func newLayout(t *testing.T) *layoutBuilder {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &layoutBuilder{t: t, dir: dir}
}

// blob stores data and returns its digest.
func (l *layoutBuilder) blob(data []byte) string {
	l.t.Helper()
	h := sha256Hex(data)
	if err := os.WriteFile(filepath.Join(l.dir, "blobs", "sha256", h), data, 0o644); err != nil {
		l.t.Fatal(err)
	}
	return "sha256:" + h
}

func (l *layoutBuilder) json(v any) []byte {
	l.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		l.t.Fatal(err)
	}
	return b
}

type platform struct{ OS, Arch, Variant string }

// image stores a manifest blob and returns its descriptor with the platform.
func (l *layoutBuilder) image(p *platform, seed string) map[string]any {
	l.t.Helper()
	manifest := l.json(map[string]any{"schemaVersion": 2, "mediaType": mediaManifest, "annotations": map[string]string{"seed": seed}})
	d := map[string]any{"mediaType": mediaManifest, "size": len(manifest), "digest": l.blob(manifest)}
	if p != nil {
		pm := map[string]string{"os": p.OS, "architecture": p.Arch}
		if p.Variant != "" {
			pm["variant"] = p.Variant
		}
		d["platform"] = pm
	}
	return d
}

// index stores an image index with the given children and returns its descriptor.
func (l *layoutBuilder) index(children ...map[string]any) map[string]any {
	l.t.Helper()
	doc := l.json(map[string]any{"schemaVersion": 2, "mediaType": mediaIndex, "manifests": children})
	return map[string]any{"mediaType": mediaIndex, "size": len(doc), "digest": l.blob(doc)}
}

func (l *layoutBuilder) writeTop(descriptors ...map[string]any) {
	l.t.Helper()
	top := l.json(map[string]any{"schemaVersion": 2, "mediaType": mediaIndex, "manifests": descriptors})
	if err := os.WriteFile(filepath.Join(l.dir, "index.json"), top, 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func assertScript(t *testing.T) string {
	t.Helper()
	needTools(t, "bash", "python3")
	return scriptPath(t, "scripts/assert-image-platforms.sh")
}

var releasePlatforms = []platform{
	{"linux", "amd64", ""},
	{"linux", "arm64", "v8"}, // ko records v8 for arm64
	{"linux", "arm", "v7"},
}

func fullIndex(l *layoutBuilder, ps []platform) map[string]any {
	var kids []map[string]any
	for i := range ps {
		kids = append(kids, l.image(&ps[i], ps[i].Arch+ps[i].Variant))
	}
	return l.index(kids...)
}

// AC "a CI step asserts the index contains linux/arm/v7 by inspecting a full
// OCI index built into a disposable OCI layout".
func TestAssertImagePlatformsAcceptsTheFullIndex(t *testing.T) {
	script := assertScript(t)
	l := newLayout(t)
	l.writeTop(fullIndex(l, releasePlatforms))
	res := run(t, "", nil, "bash", script, "--layout", l.dir)
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.combined())
	}
	mustContain(t, "output", res.stdout, "found linux/amd64, linux/arm/v7, linux/arm64", "ok")
}

func TestAssertImagePlatformsFailures(t *testing.T) {
	script := assertScript(t)
	tests := []struct {
		name  string
		build func(l *layoutBuilder)
		args  []string
		want  string
	}{
		{
			name: "arm/v7 missing",
			build: func(l *layoutBuilder) {
				l.writeTop(fullIndex(l, releasePlatforms[:2]))
			},
			want: "missing platform(s): linux/arm/v7",
		},
		{
			name: "arm64 only at a different variant name does not satisfy arm/v7",
			build: func(l *layoutBuilder) {
				ps := []platform{{"linux", "amd64", ""}, {"linux", "arm64", "v8"}, {"linux", "arm", "v6"}}
				l.writeTop(fullIndex(l, ps))
			},
			want: "missing platform(s): linux/arm/v7",
		},
		{
			name: "single image instead of an index (what a snapshot loads)",
			build: func(l *layoutBuilder) {
				l.writeTop(l.image(&platform{"linux", "amd64", ""}, "solo"))
			},
			want: "no image index",
		},
		{
			name: "platform entry points at a blob that is not in the layout",
			build: func(l *layoutBuilder) {
				// an index that references a manifest blob which was never written
				ghost := map[string]any{
					"mediaType": mediaManifest, "size": 1,
					"digest":   "sha256:" + strings.Repeat("0", 64),
					"platform": map[string]string{"os": "linux", "architecture": "arm", "variant": "v7"},
				}
				kids := []map[string]any{
					l.image(&releasePlatforms[0], "a"), l.image(&releasePlatforms[1], "b"), ghost,
				}
				l.writeTop(l.index(kids...))
			},
			want: "is missing from the layout",
		},
		{
			name:  "no index.json",
			build: func(l *layoutBuilder) {},
			want:  "cannot read index.json",
		},
		{
			name: "custom platform list",
			build: func(l *layoutBuilder) {
				l.writeTop(fullIndex(l, releasePlatforms))
			},
			args: []string{"--platforms", "linux/amd64,linux/riscv64"},
			want: "missing platform(s): linux/riscv64",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newLayout(t)
			tc.build(l)
			args := append([]string{script, "--layout", l.dir}, tc.args...)
			res := run(t, "", nil, "bash", args...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1: %s", res.code, res.combined())
			}
			mustContain(t, "stderr", res.stderr, tc.want)
		})
	}
}

func TestAssertImagePlatformsIgnoresAttestationsAndNesting(t *testing.T) {
	script := assertScript(t)
	l := newLayout(t)
	att := l.image(&platform{"unknown", "unknown", ""}, "attestation")
	inner := fullIndex(l, releasePlatforms)
	// the full index is reached through a second index level and the attestation
	// entry must not count as a platform
	l.writeTop(l.index(inner, att))
	res := run(t, "", nil, "bash", script, "--layout", l.dir)
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.combined())
	}
	if strings.Contains(res.stdout, "unknown") {
		t.Errorf("attestation counted as a platform: %s", res.stdout)
	}
}

func TestAssertImagePlatformsUsage(t *testing.T) {
	script := assertScript(t)
	for _, args := range [][]string{{"--nope"}, {"--layout"}, {"--layout", "/does/not/exist"}} {
		res := run(t, "", nil, "bash", append([]string{script}, args...)...)
		if res.code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", args, res.code, res.combined())
		}
	}
	// Without --layout the script needs ko; a missing ko is a tool error, never a pass.
	bin := t.TempDir()
	if err := os.Symlink(mustLookPath(t, "python3"), filepath.Join(bin, "python3")); err != nil {
		t.Fatal(err)
	}
	res := run(t, "", []string{"PATH=" + bin}, mustLookPath(t, "bash"), script)
	if res.code != 2 || !strings.Contains(res.stderr, "ko is required") {
		t.Errorf("without ko: exit %d: %s", res.code, res.combined())
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	needTools(t, name)
	p, err := lookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
