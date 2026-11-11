package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPipeAcceptFromAndCallKeySettingsAreDeclared(t *testing.T) {
	for key, flag := range map[string]string{KeyCallAPIKeyFile: "call-api-key-path", KeyPipeAcceptFrom: "pipe-accept-from"} {
		s, ok := Default.Lookup(key)
		if !ok || s.Scope != ScopeWorker || !s.HasFlag() || s.FlagName() != flag || s.Secret || s.Help == "" {
			t.Errorf("%s: %+v (declared %v)", key, s, ok)
		}
	}
	if s, _ := Default.Lookup(KeyPipeAcceptFrom); s.Default != "none" {
		t.Errorf("pipe_accept_from defaults to %q, want none", s.Default)
	}
}

func TestParsePipeAcceptFrom(t *testing.T) {
	fp := strings.Repeat("a", 52)
	id := "01890a5d-ac96-774b-bcce-b302099a8057"
	ok := map[string]PipeAcceptFrom{
		"":              {},
		"none":          {},
		" NONE ":        {},
		"owner-workers": {OwnerWorkers: true},
		"Owner-Workers": {OwnerWorkers: true},
		id:              {Allow: []string{id}},
		id + ", " + strings.ToUpper(fp) + "," + id: {Allow: []string{id, fp}},
	}
	for in, want := range ok {
		got, err := ParsePipeAcceptFrom(in)
		if err != nil || got.OwnerWorkers != want.OwnerWorkers || !slices.Equal(got.Allow, want.Allow) {
			t.Errorf("%q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{",", id + ",", "none," + id, "owner-workers," + id, "not a worker", "w;rm -rf", strings.Repeat("a", 51), strings.Repeat("a", 65), strings.Repeat("a", 51) + "b", strings.Repeat("a", 51) + "1", "01890a5d-ac96-774b-bcce-b302099a805", "*", "all"} {
		if _, err := ParsePipeAcceptFrom(in); err == nil {
			t.Errorf("%q was accepted", in)
		} else if strings.Contains(err.Error(), in) && in != "" {
			t.Errorf("the error repeats the value %q: %v", in, err)
		}
	}
	if (PipeAcceptFrom{}).String() != "none" || (PipeAcceptFrom{OwnerWorkers: true}).String() != "owner-workers" {
		t.Error("String() of none / owner-workers")
	}
}

func TestResolveWorkerCoreCallKeyAndPipeAccept(t *testing.T) {
	dir := t.TempDir()
	wc, err := ResolveWorkerCore(workerResolver(nil, map[string]string{"MINION_STATE_DIR": dir}, nil))
	if err != nil || wc.CallAPIKeyFile != "" || wc.PipeAcceptFrom.String() != "none" {
		t.Fatalf("defaults: %+v, %v", wc, err)
	}
	rel := filepath.Join("keys", "call.key")
	abs, _ := filepath.Abs(rel)
	wc, err = ResolveWorkerCore(workerResolver(
		map[string]string{KeyPipeAcceptFrom: "owner-workers"},
		map[string]string{"MINION_STATE_DIR": dir, "MINION_CALL_API_KEY_PATH": rel},
		nil))
	if err != nil || wc.CallAPIKeyFile != abs || !wc.PipeAcceptFrom.OwnerWorkers {
		t.Fatalf("set: %+v, %v", wc, err)
	}
	// The setting also comes from the config file (minion worker config set).
	wc, err = ResolveWorkerCore(workerResolver(nil, map[string]string{"MINION_STATE_DIR": dir}, map[string]string{KeyCallAPIKeyFile: abs, KeyPipeAcceptFrom: "01890a5d-ac96-774b-bcce-b302099a8057"}))
	if err != nil || wc.CallAPIKeyFile != abs || len(wc.PipeAcceptFrom.Allow) != 1 {
		t.Fatalf("file: %+v, %v", wc, err)
	}
	if _, err := ResolveWorkerCore(workerResolver(nil, map[string]string{"MINION_STATE_DIR": dir, "MINION_PIPE_ACCEPT_FROM": "everyone"}, nil)); err == nil || !strings.Contains(err.Error(), KeyPipeAcceptFrom) {
		t.Errorf("a bad value: err = %v", err)
	}
}
