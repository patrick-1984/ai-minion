package releasecheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type manifestDoc struct {
	Schema        int `json:"schema"`
	Version       string
	ProtocolRange struct{ Min, Max string } `json:"protocol_range"`
	Artifacts     []struct{ Name, Sha256 string }
}

func manifestScript(t *testing.T) string {
	t.Helper()
	needTools(t, "bash", "python3", "sha256sum")
	return scriptPath(t, "scripts/release-manifest.sh")
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func writeChecksums(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "checksums.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// AC "release-manifest.json (version, protocol range, artifact digests)".
func TestReleaseManifestContent(t *testing.T) {
	script := manifestScript(t)
	sums := writeChecksums(t,
		shaB+"  minion_0.1.0_windows_amd64_preview.zip",
		shaA+"  minion_0.1.0_linux_amd64.tar.gz",
		shaA+" *minion-0.1.0-1.x86_64.rpm", // binary-mode marker is accepted
	)
	extra := filepath.Join(t.TempDir(), "bootstrap.sh")
	body := []byte("#!/bin/sh\necho hi\n")
	if err := os.WriteFile(extra, body, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "release-manifest.json")
	res := run(t, "", nil, "bash", script, "--version", "v0.1.0-rc.1",
		"--protocol-min", "1.0", "--protocol-max", "1.2",
		"--checksums", sums, "--extra", extra, "--output", out)
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.combined())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m manifestDoc
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	if m.Schema != 1 || m.Version != "v0.1.0-rc.1" || m.ProtocolRange.Min != "1.0" || m.ProtocolRange.Max != "1.2" {
		t.Errorf("header = %+v", m)
	}
	want := []struct{ Name, Sha256 string }{
		{"bootstrap.sh", sha256Hex(body)},
		{"minion-0.1.0-1.x86_64.rpm", shaA},
		{"minion_0.1.0_linux_amd64.tar.gz", shaA},
		{"minion_0.1.0_windows_amd64_preview.zip", shaB},
	}
	if len(m.Artifacts) != len(want) {
		t.Fatalf("artifacts = %+v", m.Artifacts)
	}
	for i := range want {
		if m.Artifacts[i] != want[i] { // sorted by name
			t.Errorf("artifact %d = %+v, want %+v", i, m.Artifacts[i], want[i])
		}
	}
	// deterministic: a second run produces the same bytes
	out2 := filepath.Join(t.TempDir(), "again.json")
	run(t, "", nil, "bash", script, "--version", "v0.1.0-rc.1", "--protocol-min", "1.0", "--protocol-max", "1.2",
		"--checksums", sums, "--extra", extra, "--output", out2)
	raw2, _ := os.ReadFile(out2)
	if string(raw) != string(raw2) {
		t.Error("the manifest is not deterministic")
	}
}

func TestReleaseManifestStdoutAndDefaultName(t *testing.T) {
	script := manifestScript(t)
	sums := writeChecksums(t, shaA+"  minion_1.0.0_linux_amd64.tar.gz")
	res := run(t, "", nil, "bash", script, "--version", "v1.0.0", "--protocol-min", "1.0", "--protocol-max", "1.0",
		"--checksums", sums, "--output", "-")
	if res.code != 0 || !json.Valid([]byte(res.stdout)) {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	dir := t.TempDir()
	res = run(t, dir, nil, "bash", script, "--version", "v1.0.0", "--protocol-min", "1.0", "--protocol-max", "1.0", "--checksums", sums)
	if res.code != 0 {
		t.Fatal(res.combined())
	}
	if _, err := os.Stat(filepath.Join(dir, "release-manifest.json")); err != nil {
		t.Errorf("default output name: %v", err)
	}
}

func TestReleaseManifestRejectsBadInput(t *testing.T) {
	script := manifestScript(t)
	good := writeChecksums(t, shaA+"  minion_0.1.0_linux_amd64.tar.gz")
	base := func(over map[string]string) []string {
		vals := map[string]string{"--version": "v0.1.0", "--protocol-min": "1.0", "--protocol-max": "1.0", "--checksums": good, "--output": "-"}
		for k, v := range over {
			vals[k] = v
		}
		var args []string
		for k, v := range vals {
			if v != "" {
				args = append(args, k, v)
			}
		}
		return args
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"version without v", base(map[string]string{"--version": "0.1.0"}), "version must be"},
		{"version with a build suffix", base(map[string]string{"--version": "v0.1.0-beta.1"}), "version must be"},
		{"leading zero", base(map[string]string{"--version": "v01.1.0"}), "version must be"},
		{"missing version", base(map[string]string{"--version": ""}), "--version is required"},
		{"missing protocol min", base(map[string]string{"--protocol-min": ""}), "--protocol-min is required"},
		{"missing checksums", base(map[string]string{"--checksums": ""}), "--checksums is required"},
		{"checksums file absent", base(map[string]string{"--checksums": "/does/not/exist"}), "no such file"},
		{"protocol not major.minor", base(map[string]string{"--protocol-max": "1"}), "must be MAJOR.MINOR"},
		{"protocol min above max", base(map[string]string{"--protocol-min": "1.3", "--protocol-max": "1.2"}), "greater than"},
		{"protocol range across majors", base(map[string]string{"--protocol-min": "1.0", "--protocol-max": "2.0"}), "one major"},
		{"empty checksums", base(map[string]string{"--checksums": writeChecksums(t)}), "no artifacts"},
		{"malformed line", base(map[string]string{"--checksums": writeChecksums(t, "nothex  file.tar.gz")}), "malformed checksum line"},
		{"path in name", base(map[string]string{"--checksums": writeChecksums(t, shaA+"  ../evil")}), "plain file name"},
		{"conflicting digests", base(map[string]string{"--checksums": writeChecksums(t, shaA+"  a.tar.gz", shaB+"  a.tar.gz")}), "conflicting digests"},
		{"extra file absent", append(base(nil), "--extra", "/does/not/exist"), "no such extra file"},
		{"unknown flag", append(base(nil), "--nope"), "unknown argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, "", nil, "bash", append([]string{script}, tc.args...)...)
			if res.code != 2 {
				t.Fatalf("exit %d, want 2 (stdout %q)", res.code, res.stdout)
			}
			if !strings.Contains(res.stderr, tc.want) {
				t.Errorf("stderr %q lacks %q", res.stderr, tc.want)
			}
			if res.stdout != "" {
				t.Errorf("a failing run must not print a manifest: %q", res.stdout)
			}
		})
	}
}
