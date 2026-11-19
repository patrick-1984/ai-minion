package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNextJoinPin(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		prev               JoinPin
		hadPrev            bool
		fileServer, fileFp string
		server, fp         string
		wantServer, wantFp bool
		wantFpValue        string
	}{
		"fresh":             {server: "s", fp: "F", wantServer: true, wantFp: true, wantFpValue: "F"},
		"provisioned fp":    {fileFp: "F", server: "s", fp: "F", wantServer: true, wantFpValue: "F"},
		"provisioned no fp": {fileServer: "s0", fileFp: "F", server: "s", wantFpValue: "F"},
		"earlier join": {prev: JoinPin{ServerCreated: true, Server: "s", HubFingerprintCreated: true, HubFingerprint: "F"}, hadPrev: true,
			fileServer: "s", fileFp: "F", server: "s", fp: "F", wantServer: true, wantFp: true, wantFpValue: "F"},
		"edited after join": {prev: JoinPin{HubFingerprintCreated: true, HubFingerprint: "F"}, hadPrev: true,
			fileServer: "s", fileFp: "G", server: "s", wantFpValue: "G"},
		"no fp at all": {server: "s", wantServer: true},
	} {
		got := NextJoinPin(tc.prev, tc.hadPrev, tc.fileServer, tc.fileFp, tc.server, tc.fp, now)
		if got.ServerCreated != tc.wantServer || got.HubFingerprintCreated != tc.wantFp || got.HubFingerprint != tc.wantFpValue {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestJoinPinRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadJoinPin(dir); ok || err != nil {
		t.Fatalf("empty dir: %v %v", ok, err)
	}
	want := JoinPin{Version: JoinPinVersion, Source: JoinPinSourceBackground, Server: "https://h", ServerCreated: true,
		HubFingerprint: "F", HubFingerprintCreated: true, At: time.Now().UTC().Truncate(time.Second)}
	if err := WriteJoinPin(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadJoinPin(dir)
	if err != nil || !ok || got != want {
		t.Fatalf("read = %+v, %v, %v", got, ok, err)
	}
	if !got.CreatedFingerprint("F") || got.CreatedFingerprint("G") || !got.CreatedServer("https://h") {
		t.Fatalf("created checks: %+v", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, JoinPinFile)); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("mode: %v %v", fi, err)
	}
	if err := os.WriteFile(filepath.Join(dir, JoinPinFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadJoinPin(dir); ok || err == nil {
		t.Fatal("a broken record must be an error")
	}
	if err := RemoveJoinPin(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveJoinPin(dir); err != nil {
		t.Fatal(err)
	}
}
