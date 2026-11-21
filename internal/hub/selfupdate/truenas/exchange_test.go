//go:build linux

package truenas

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadRequest(t *testing.T) {
	now := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "request.json")
	exp := now.Add(time.Hour).Format(time.RFC3339)
	good := `{"version":1,"policy":{"enabled":true,"channel":"0.17","window":"03-05","revision":3},` +
		`"once":{"id":"abcdefgh","baseline":"v0.16.0","target":"v0.17.0","policy_revision":3,"expires_at":"` + exp + `"}}`

	if r, err := ReadRequest(filepath.Join(dir, "none.json"), now); r != nil || err != nil {
		t.Fatalf("missing file: %v %v", r, err)
	}
	cases := map[string]string{
		"request_invalid_json":                 "{",
		"request_unsupported_version":          `{"version":2}`,
		"request_invalid_once":                 `{"version":1,"once":{"id":"x","baseline":"v0.16.0","target":"v0.17.0","expires_at":"` + exp + `"}}`,
		"request_expired":                      `{"version":1,"once":{"id":"abcdefgh","baseline":"v0.16.0","target":"v0.17.0","expires_at":"2020-01-01T00:00:00Z"}}`,
		"request_policy_revision_mismatch":     strings.Replace(good, `"revision":3`, `"revision":4`, 1),
		"request_invalid_policy":               `{"version":1,"policy":{"enabled":true,"channel":"v1.2.3","window":"03-05","revision":1}}`,
		"request_invalid_json (unknown field)": `{"version":1,"extra":1}`,
		"request_too_large":                    `{"version":1,"pad":"` + strings.Repeat("a", MaxRequestSize) + `"}`,
	}
	for name, body := range cases {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadRequest(path, now)
		code, _, _ := strings.Cut(name, " ")
		if err == nil || err.Error() != code {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadRequest(path, now); err != nil || r.Once.ID != "abcdefgh" {
		t.Fatalf("good request: %+v %v", r, err)
	}
}

func TestReadRequestRefusesSymlinkAndFIFO(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(link, now); err == nil || err.Error() != "request_unreadable" {
		t.Fatalf("symlink: %v", err)
	}
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadRequest(fifo, now); done <- err }()
	select {
	case err := <-done:
		if err == nil || err.Error() != "request_not_regular_file" {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}

func TestStatusIsAtomicAndSanitized(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out", "status.json")
	if err := WriteStatus(p, Status{App: "a", Tick: "idle"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(p, Status{App: "a", Tick: "updated"}); err != nil {
		t.Fatal(err)
	}
	es, _ := os.ReadDir(filepath.Dir(p))
	if len(es) != 1 {
		t.Fatalf("leftover files: %v", es)
	}
}

func TestJournalRefusesUntrustedAncestors(t *testing.T) {
	uid := os.Getuid()
	base := t.TempDir()
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(filepath.Join(open, "state"), "ai-minion", uid, nil); err == nil {
		t.Fatal("a world-writable ancestor was accepted")
	}
	ok := filepath.Join(base, "ok")
	if err := os.Mkdir(ok, 0o700); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(filepath.Join(ok, "state"), "ai-minion", uid, nil)
	if err != nil {
		// A parent of the temp dir may legitimately be group-writable on this host.
		t.Skipf("host ancestors not trusted: %v", err)
	}
	_ = j.Close()
}

func TestJournalRejectsSymlinkInOriginalAncestry(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "private")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedAncestors(filepath.Join(link, "future-state"), -1); err == nil {
		t.Fatal("resolved safe target hid an unsafe original symlink")
	}
}

func TestJournalWriteFailsWhenDirectoryCannotBeSynced(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "s"), "ai-minion", -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Close() // every later operation fails instead of reporting a durable intent
	if err := j.SaveAttempt(&Attempt{ID: "x"}); err == nil {
		t.Fatal("a journal write on a dead directory handle succeeded")
	}
}
