package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

// RC1 release gate, finding 6: restore names the schema of the archive and the
// schema of the installed minion separately, instead of printing the archive's
// schema as if it were the installed one.
func TestRunHubRestorePrintsArchiveAndInstalledSchema(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "hub")
	s, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	archive := filepath.Join(t.TempDir(), "hub.tar.gz")
	if err := runHubBackup(ctx, &bytes.Buffer{}, hubBackupDeps{}, src, archive, plainBackup); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	hostOnly := hubBackupDeps{containerDir: func(string) containerKind { return notContainer }}
	var out bytes.Buffer
	if err := runHubRestore(ctx, &out, hostOnly, dst, archive, false, "", 0); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`archive schema (\d+), written by minion [^;]+; installed schema (\d+)`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("output does not label both schemas: %q", out.String())
	}
	archived, _ := strconv.Atoi(m[1])
	installed, _ := strconv.Atoi(m[2])
	// The archive was written by this very build: both are the newest schema.
	if archived <= 0 || installed != archived {
		t.Errorf("archive schema %d, installed schema %d", archived, installed)
	}
}
