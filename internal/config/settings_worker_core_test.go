package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkerCoreSettingsDeclared(t *testing.T) {
	want := map[string]string{
		KeyTmuxSocket:    "tmux-socket",
		KeyName:          "name",
		KeyShareExisting: "share-existing",
		KeyPreviewOnly:   "preview-only",
	}
	for key, flag := range want {
		s, ok := Default.Lookup(key)
		if !ok {
			t.Fatalf("%s not declared", key)
		}
		if s.Scope != ScopeWorker || !s.HasFlag() || s.FlagName() != flag || s.Help == "" {
			t.Errorf("%s: scope %s flag %q help %q", key, s.Scope, s.FlagName(), s.Help)
		}
	}
}

func TestPathEnvTmuxSocket(t *testing.T) {
	cases := []struct {
		name string
		env  PathEnv
		want string
	}{
		{"default", PathEnv{GOOS: "linux", UID: 1000, Getenv: func(string) string { return "" }}, "/tmp/tmux-1000/default"},
		{"tmux tmpdir", PathEnv{GOOS: "linux", UID: 7, Getenv: func(k string) string {
			if k == "TMUX_TMPDIR" {
				return "/run/user/7"
			}
			return ""
		}}, "/run/user/7/tmux-7/default"},
		{"darwin", PathEnv{GOOS: "darwin", UID: 501, Getenv: func(string) string { return "" }}, "/tmp/tmux-501/default"},
		{"windows has no tmux", PathEnv{GOOS: "windows", Getenv: func(string) string { return "" }}, ""},
	}
	for _, c := range cases {
		want := c.want
		if want != "" {
			// The expectations are slash paths; make them what this platform
			// calls an absolute path (a drive letter and backslashes on Windows).
			want, _ = filepath.Abs(filepath.FromSlash(want))
		}
		got, err := c.env.TmuxSocket()
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, want)
		}
	}
	rel := PathEnv{GOOS: "linux", UID: 1, Getenv: func(k string) string {
		if k == "TMUX_TMPDIR" {
			return "rel"
		}
		return ""
	}}
	got, err := rel.TmuxSocket()
	if err != nil || !filepath.IsAbs(got) || !strings.HasSuffix(got, filepath.Join("rel", "tmux-1", "default")) {
		t.Errorf("relative TMUX_TMPDIR: %q, %v", got, err)
	}
}

func workerResolver(flags, env, file map[string]string) *Resolver {
	r := &Resolver{Role: ScopeWorker, LookupEnv: envMap(env)}
	if flags != nil {
		r.Flags = MapSource(flags)
	}
	if file != nil {
		r.File = MapSource(file)
	}
	return r
}

func TestResolveWorkerCoreDefaults(t *testing.T) {
	dir := t.TempDir()
	wc, err := ResolveWorkerCore(workerResolver(nil, map[string]string{"MINION_STATE_DIR": dir}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if wc.StateDir != dir || wc.ShareExisting != "" || wc.PreviewOnly || wc.Name == "" {
		t.Errorf("core = %+v", wc)
	}
	if runtime.GOOS == "windows" {
		if wc.TmuxSocket != "" {
			t.Errorf("default socket %q, want none on Windows (ConPTY hosts the sessions)", wc.TmuxSocket)
		}
	} else if !filepath.IsAbs(wc.TmuxSocket) || !strings.HasSuffix(wc.TmuxSocket, filepath.Join("default")) {
		t.Errorf("default socket %q is not an absolute default socket path", wc.TmuxSocket)
	}
}

func TestResolveWorkerCorePrecedenceAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join("some", "dir", "tmux.sock")
	abs, _ := filepath.Abs(rel)

	// Flag beats env beats file.
	wc, err := ResolveWorkerCore(workerResolver(
		map[string]string{KeyTmuxSocket: rel, KeyName: "from-flag"},
		map[string]string{"MINION_STATE_DIR": dir, "MINION_NAME": "from-env", "MINION_SHARE_EXISTING": "none", "MINION_PREVIEW_ONLY": "false"},
		map[string]string{KeyName: "from-file", KeyShareExisting: "all", KeyPreviewOnly: "true"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if wc.TmuxSocket != abs {
		t.Errorf("socket = %q, want the absolute %q", wc.TmuxSocket, abs)
	}
	if wc.Name != "from-flag" || wc.ShareExisting != "none" || wc.PreviewOnly {
		t.Errorf("core = %+v", wc)
	}

	// File applies when nothing above sets it.
	wc, err = ResolveWorkerCore(workerResolver(nil,
		map[string]string{"MINION_STATE_DIR": dir},
		map[string]string{KeyShareExisting: "later", KeyPreviewOnly: "true", KeyName: "  padded  "}))
	if err != nil {
		t.Fatal(err)
	}
	if wc.ShareExisting != "later" || !wc.PreviewOnly || wc.Name != "padded" {
		t.Errorf("core = %+v", wc)
	}
}

func TestResolveWorkerCoreRelativeStateDir(t *testing.T) {
	wc, err := ResolveWorkerCore(workerResolver(map[string]string{KeyStateDir: "state"}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(wc.StateDir) {
		t.Errorf("state dir %q is not absolute", wc.StateDir)
	}
}

func TestResolveWorkerCoreRejectsBadValues(t *testing.T) {
	dir := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	cases := []struct {
		name  string
		flags map[string]string
		want  string
	}{
		{"share", map[string]string{KeyShareExisting: "everything"}, "share_existing"},
		{"preview", map[string]string{KeyPreviewOnly: "maybe"}, "preview_only"},
		{"empty name", map[string]string{KeyName: "   "}, "empty"},
		{"long name", map[string]string{KeyName: strings.Repeat("x", MaxNameLength+1)}, "longer"},
		{"control name", map[string]string{KeyName: "a\nb"}, "control"},
		{"empty socket", map[string]string{KeyTmuxSocket: ""}, "tmux_socket"},
	}
	for _, c := range cases {
		if c.name == "empty socket" && runtime.GOOS == "windows" {
			continue // ConPTY hosts the sessions; there is no socket
		}
		_, err := ResolveWorkerCore(workerResolver(c.flags, dir, nil))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		}
	}
}
