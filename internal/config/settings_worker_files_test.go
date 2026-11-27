package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNarrowFileTransfer(t *testing.T) {
	cases := map[[2]string]string{
		{"on", "on"}: "on", {"on", "send_only"}: "send_only", {"send_only", "receive_only"}: "off",
		{"receive_only", "on"}: "receive_only", {"off", "on"}: "off", {"bogus", "on"}: "off",
	}
	for in, want := range cases {
		if got := NarrowFileTransfer(in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}

func TestResolveFileTransferFileNarrowsEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, WorkerConfigFile)
	env := map[string]string{}
	res := &Resolver{Role: ScopeWorker, Registry: Default, LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	if got := ResolveFileTransfer(res, path); got != FileTransferOn {
		t.Fatalf("default: %s", got)
	}
	if err := os.WriteFile(path, []byte(`{"file_transfer":"send_only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env["MINION_FILE_TRANSFER"] = "on"
	if got := ResolveFileTransfer(res, path); got != FileTransferSendOnly {
		t.Fatalf("file send_only, env on: %s", got)
	}
	env["MINION_FILE_TRANSFER"] = "receive_only"
	if got := ResolveFileTransfer(res, path); got != FileTransferOff {
		t.Fatalf("file send_only, env receive_only: %s", got)
	}
	if err := os.WriteFile(path, []byte(`{"file_transfer_protected":"on"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env["MINION_FILE_TRANSFER_PROTECTED"] = "off"
	if ResolveFileTransferLocal(res, path).ProtectedOff {
		t.Fatal("the environment relaxed a protection the file keeps")
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveFileTransfer(res, path); got != FileTransferOff {
		t.Fatalf("unreadable file: %s", got)
	}
}
