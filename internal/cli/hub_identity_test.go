package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
)

func TestHubIdentityIsRegisteredAndTakesNoArguments(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"hub", "identity"})
	if err != nil || cmd.Name() != "identity" {
		t.Fatalf("hub identity not found: %v", err)
	}
	var out, errb bytes.Buffer
	if code := Execute(context.Background(), []string{"hub", "identity", "extra"}, &out, &errb); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestHubIdentityPrintsTheFingerprintOfTheKeyInTheDataDir(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := workers.LoadOrCreateIdentity(context.Background(), dir, st, nil)
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir})); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out.String())
	if got != id.Fingerprint().String() || len(got) != 52 {
		t.Errorf("printed %q, want %q", got, id.Fingerprint())
	}
	// Reading never changes the identity.
	var again bytes.Buffer
	if err := runHubIdentity(&again, envOf(map[string]string{"MINION_DATA_DIR": dir})); err != nil || again.String() != out.String() {
		t.Errorf("second run printed %q err=%v", again.String(), err)
	}
}

func TestHubIdentityBeforeTheFirstStartSaysSoAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runHubIdentity(&out, envOf(map[string]string{"MINION_DATA_DIR": dir}))
	if err == nil || !strings.Contains(err.Error(), "minion hub serve") {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(matches) != 0 {
		t.Errorf("the command created %v", matches)
	}
}
