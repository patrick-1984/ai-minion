package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func fileMap(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("open " + p + ": no such file or directory")
	}
}

type errSource struct{}

func (errSource) Lookup(string) (string, bool, error) { return "", false, errors.New("boom") }

func hubRegistry() *Registry {
	r := NewRegistry()
	r.Declare(Setting{Key: "base_url", Scope: ScopeHub, Default: "http://localhost:8080"})
	r.Declare(Setting{Key: "no_default", Scope: ScopeHub})
	r.Declare(Setting{Key: "admin_password", Scope: ScopeHub, Secret: true, PlainEnvAllowed: true})
	r.Declare(Setting{Key: "oidc_client_secret", Scope: ScopeHub, Secret: true})
	r.Declare(Setting{Key: "worker_only", Scope: ScopeWorker})
	return r
}

func TestHubPrecedenceMatrix(t *testing.T) {
	type tc struct {
		name      string
		env       map[string]string
		panel     MapSource
		wantValue string
		wantSrc   Source
		readOnly  bool
	}
	cases := []tc{
		{"default only", nil, nil, "http://localhost:8080", SourceDefault, false},
		{"panel over default", nil, MapSource{"base_url": "http://panel"}, "http://panel", SourcePanel, false},
		{"env over default", map[string]string{"MINION_BASE_URL": "http://env"}, nil, "http://env", SourceEnv, true},
		{"env over panel over default", map[string]string{"MINION_BASE_URL": "http://env"}, MapSource{"base_url": "http://panel"}, "http://env", SourceEnv, true},
		{"empty env is unset", map[string]string{"MINION_BASE_URL": ""}, MapSource{"base_url": "http://panel"}, "http://panel", SourcePanel, false},
		{"panel can hold an empty value", nil, MapSource{"base_url": ""}, "", SourcePanel, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Resolver{Registry: hubRegistry(), Role: ScopeHub, LookupEnv: envMap(c.env)}
			if c.panel != nil {
				r.Panel = c.panel
			}
			got, err := r.Resolve("base_url")
			if err != nil {
				t.Fatal(err)
			}
			if got.Value != c.wantValue || got.Source != c.wantSrc || got.ReadOnly() != c.readOnly {
				t.Fatalf("got %q from %s (readOnly %v), want %q from %s (readOnly %v)",
					got.Value, got.Source, got.ReadOnly(), c.wantValue, c.wantSrc, c.readOnly)
			}
		})
	}
}

func TestHubIgnoresFlagsAndFile(t *testing.T) {
	r := &Resolver{
		Registry:  hubRegistry(),
		Role:      ScopeHub,
		LookupEnv: envMap(nil),
		Flags:     MapSource{"base_url": "http://flag"},
		File:      MapSource{"base_url": "http://file"},
	}
	got, err := r.Resolve("base_url")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceDefault {
		t.Fatalf("hub read %s", got.Source)
	}
}

func workerRegistry() *Registry {
	r := NewRegistry()
	r.Declare(Setting{Key: "server", Scope: ScopeWorker, Default: "wss://default"})
	r.Declare(Setting{Key: "invite_key", Scope: ScopeWorker, Secret: true, ArgvAllowedWithWarning: true})
	r.Declare(Setting{Key: "token", Scope: ScopeWorker, Secret: true})
	r.Declare(Setting{Key: "hub_only", Scope: ScopeHub})
	return r
}

func TestWorkerPrecedenceMatrix(t *testing.T) {
	flag := MapSource{"server": "flag"}
	file := MapSource{"server": "file"}
	env := map[string]string{"MINION_SERVER": "env"}
	cases := []struct {
		name    string
		flags   FlagSource
		env     map[string]string
		file    FileSource
		want    string
		wantSrc Source
	}{
		{"default", nil, nil, nil, "wss://default", SourceDefault},
		{"file over default", nil, nil, file, "file", SourceFile},
		{"env over file", nil, env, file, "env", SourceEnv},
		{"flag over env over file", flag, env, file, "flag", SourceFlag},
		{"flag over default", flag, nil, nil, "flag", SourceFlag},
		{"flag over file", flag, nil, file, "flag", SourceFlag},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(c.env), Flags: c.flags, File: c.file}
			got, err := r.Resolve("server")
			if err != nil {
				t.Fatal(err)
			}
			if got.Value != c.want || got.Source != c.wantSrc {
				t.Fatalf("got %q from %s, want %q from %s", got.Value, got.Source, c.want, c.wantSrc)
			}
			if got.ReadOnly() != (c.wantSrc == SourceEnv) {
				t.Fatalf("ReadOnly = %v", got.ReadOnly())
			}
		})
	}
}

func TestWorkerIgnoresPanel(t *testing.T) {
	r := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(nil), Panel: MapSource{"server": "panel"}}
	got, _ := r.Resolve("server")
	if got.Source != SourceDefault {
		t.Fatalf("worker read %s", got.Source)
	}
}

func TestSecretFromFileEnv(t *testing.T) {
	files := map[string]string{
		"/run/secrets/pw":      "s3cret\n",
		"/run/secrets/pw-crlf": "s3cret\r\n",
		"/run/secrets/bare":    "s3cret",
		"/run/secrets/two":     "s3cret\n\n",
		"/run/secrets/empty":   "",
		"/run/secrets/nl":      "\n",
	}
	cases := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{"trailing newline trimmed", "/run/secrets/pw", "s3cret", ""},
		{"crlf trimmed", "/run/secrets/pw-crlf", "s3cret", ""},
		{"no newline", "/run/secrets/bare", "s3cret", ""},
		{"only one newline trimmed", "/run/secrets/two", "s3cret\n", ""},
		{"missing file", "/run/secrets/missing", "", "cannot read secret file"},
		{"empty file", "/run/secrets/empty", "", "is empty"},
		{"newline only file", "/run/secrets/nl", "", "is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Resolver{
				Registry:  hubRegistry(),
				Role:      ScopeHub,
				LookupEnv: envMap(map[string]string{"MINION_OIDC_CLIENT_SECRET_FILE": c.path}),
				ReadFile:  fileMap(files),
			}
			got, err := r.Resolve("oidc_client_secret")
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, c.wantErr)
				}
				if !strings.Contains(err.Error(), "MINION_OIDC_CLIENT_SECRET_FILE") {
					t.Fatalf("error does not name the variable: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Value != c.want || got.Source != SourceEnv {
				t.Fatalf("got %q from %s", got.Value, got.Source)
			}
			if got.Display() != Redacted || strings.Contains(got.String(), "s3cret") {
				t.Fatalf("secret leaks in output: %q / %q", got.Display(), got.String())
			}
		})
	}
}

func TestEmptyFileEnvIsUnset(t *testing.T) {
	r := &Resolver{Registry: hubRegistry(), Role: ScopeHub, LookupEnv: envMap(map[string]string{"MINION_OIDC_CLIENT_SECRET_FILE": ""})}
	got, err := r.Resolve("oidc_client_secret")
	if err != nil || got.Source != SourceDefault || got.Value != "" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestPlainSecretEnvRejected(t *testing.T) {
	const secret = "hunter2-do-not-leak"
	r := &Resolver{
		Registry:  hubRegistry(),
		Role:      ScopeHub,
		LookupEnv: envMap(map[string]string{"MINION_OIDC_CLIENT_SECRET": secret}),
		Panel:     MapSource{"oidc_client_secret": "from-panel"},
	}
	_, err := r.Resolve("oidc_client_secret")
	if err == nil {
		t.Fatal("plain secret env accepted")
	}
	msg := err.Error()
	for _, want := range []string{"is a secret", "MINION_OIDC_CLIENT_SECRET is not accepted", "MINION_OIDC_CLIENT_SECRET_FILE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("error leaks the secret: %q", msg)
	}
}

func TestPlainSecretEnvExceptionAdminPassword(t *testing.T) {
	r := &Resolver{
		Registry:  hubRegistry(),
		Role:      ScopeHub,
		LookupEnv: envMap(map[string]string{"MINION_ADMIN_PASSWORD": "seed"}),
	}
	got, err := r.Resolve("admin_password")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "seed" || got.Source != SourceEnv {
		t.Fatalf("got %+v", got)
	}
	// _FILE wins over the plain variable when both are set.
	r.LookupEnv = envMap(map[string]string{"MINION_ADMIN_PASSWORD": "seed", "MINION_ADMIN_PASSWORD_FILE": "/f"})
	r.ReadFile = fileMap(map[string]string{"/f": "fromfile\n"})
	got, err = r.Resolve("admin_password")
	if err != nil || got.Value != "fromfile" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestSecretFromPanelAndFile(t *testing.T) {
	r := &Resolver{Registry: hubRegistry(), Role: ScopeHub, LookupEnv: envMap(nil), Panel: MapSource{"oidc_client_secret": "p"}}
	got, err := r.Resolve("oidc_client_secret")
	if err != nil || got.Value != "p" || got.Source != SourcePanel {
		t.Fatalf("panel: %+v %v", got, err)
	}
	w := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(nil), File: MapSource{"token": "t"}}
	got, err = w.Resolve("token")
	if err != nil || got.Value != "t" || got.Source != SourceFile {
		t.Fatalf("file: %+v %v", got, err)
	}
}

func TestSecretFlagRules(t *testing.T) {
	// A secret without the argv exception never reads a flag.
	w := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(nil), Flags: MapSource{"token": "argv-token"}}
	got, err := w.Resolve("token")
	if err != nil || got.Value != "" || got.Source != SourceDefault {
		t.Fatalf("secret flag honoured: %+v %v", got, err)
	}
	// The declared exception reads the flag and carries a warning.
	w.Flags = MapSource{"invite_key": "mnk_abc"}
	got, err = w.Resolve("invite_key")
	if err != nil || got.Value != "mnk_abc" || got.Source != SourceFlag {
		t.Fatalf("invite_key: %+v %v", got, err)
	}
	if !strings.Contains(got.Warning, "--invite-key") || !strings.Contains(got.Warning, "process list") {
		t.Fatalf("warning = %q", got.Warning)
	}
	if strings.Contains(got.Warning, "mnk_abc") || strings.Contains(got.String(), "mnk_abc") {
		t.Fatal("output leaks the key")
	}
	// A non-secret flag has no warning.
	ns := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(nil), Flags: MapSource{"server": "x"}}
	if got, _ := ns.Resolve("server"); got.Warning != "" {
		t.Fatalf("unexpected warning %q", got.Warning)
	}
}

func TestResolveErrors(t *testing.T) {
	r := &Resolver{Registry: hubRegistry(), Role: ScopeHub, LookupEnv: envMap(nil)}
	if _, err := r.Resolve("nope"); err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Errorf("unknown: %v", err)
	}
	if _, err := r.Resolve("worker_only"); err == nil || !strings.Contains(err.Error(), "cannot be resolved for the hub") {
		t.Errorf("scope mismatch: %v", err)
	}
	bad := &Resolver{Registry: hubRegistry(), LookupEnv: envMap(nil)}
	if _, err := bad.Resolve("base_url"); err == nil {
		t.Error("missing role accepted")
	}
	pe := &Resolver{Registry: hubRegistry(), Role: ScopeHub, LookupEnv: envMap(nil), Panel: errSource{}}
	if _, err := pe.Resolve("base_url"); err == nil || !strings.Contains(err.Error(), "panel") {
		t.Errorf("panel error: %v", err)
	}
	fe := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(nil), File: errSource{}}
	if _, err := fe.Resolve("server"); err == nil || !strings.Contains(err.Error(), "file") {
		t.Errorf("file error: %v", err)
	}
	fl := &Resolver{Registry: workerRegistry(), Role: ScopeWorker, LookupEnv: envMap(nil), Flags: errSource{}}
	if _, err := fl.Resolve("server"); err == nil || !strings.Contains(err.Error(), "flag") {
		t.Errorf("flag error: %v", err)
	}
}

func TestChoicesAndDefaultFunc(t *testing.T) {
	reg := NewRegistry()
	reg.Declare(Setting{Key: "mode", Scope: ScopeAny, Default: "a", Choices: []string{"a", "b"}})
	reg.Declare(Setting{Key: "dir", Scope: ScopeAny, DefaultFunc: func() (string, error) { return "/computed", nil }})
	reg.Declare(Setting{Key: "baddir", Scope: ScopeAny, DefaultFunc: func() (string, error) { return "", errors.New("no home") }})

	r := &Resolver{Registry: reg, Role: ScopeWorker, LookupEnv: envMap(map[string]string{"MINION_MODE": "c"})}
	if _, err := r.Resolve("mode"); err == nil || !strings.Contains(err.Error(), `invalid value "c"`) || !strings.Contains(err.Error(), "a, b") {
		t.Errorf("choices: %v", err)
	}
	r.LookupEnv = envMap(map[string]string{"MINION_MODE": "b"})
	if got, err := r.Resolve("mode"); err != nil || got.Value != "b" {
		t.Errorf("valid choice: %+v %v", got, err)
	}
	if got, err := r.Resolve("dir"); err != nil || got.Value != "/computed" || got.Source != SourceDefault {
		t.Errorf("DefaultFunc: %+v %v", got, err)
	}
	if _, err := r.Resolve("baddir"); err == nil || !strings.Contains(err.Error(), "no home") {
		t.Errorf("DefaultFunc error: %v", err)
	}
}

func TestResolveAll(t *testing.T) {
	r := &Resolver{
		Registry:  hubRegistry(),
		Role:      ScopeHub,
		LookupEnv: envMap(map[string]string{"MINION_OIDC_CLIENT_SECRET": "plain"}),
	}
	got, err := r.ResolveAll()
	if err == nil || !strings.Contains(err.Error(), "oidc_client_secret") {
		t.Fatalf("err = %v", err)
	}
	var keys []string
	for _, g := range got {
		keys = append(keys, g.Key())
	}
	if want := "admin_password,base_url,no_default"; strings.Join(keys, ",") != want {
		t.Fatalf("keys = %v, want %s (worker_only excluded, failing one skipped)", keys, want)
	}
}

func TestDefaultRegistryResolvesWithoutEnvLookup(t *testing.T) {
	// A nil Registry means the process-wide one; the core settings are in it.
	r := &Resolver{Role: ScopeHub, LookupEnv: envMap(nil)}
	got, err := r.Resolve(KeyLogLevel)
	if err != nil || got.Value != "info" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestOSEnvIsDefault(t *testing.T) {
	t.Setenv("MINION_LOG_LEVEL", "debug")
	r := &Resolver{Role: ScopeWorker}
	got, err := r.Resolve(KeyLogLevel)
	if err != nil || got.Value != "debug" || got.Source != SourceEnv {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestPlainSecretEnvRejectedEvenWithFileEnv(t *testing.T) {
	const secret = "PLAINLEAK-value"
	r := &Resolver{
		Registry: hubRegistry(),
		Role:     ScopeHub,
		LookupEnv: envMap(map[string]string{
			"MINION_OIDC_CLIENT_SECRET":      secret,
			"MINION_OIDC_CLIENT_SECRET_FILE": "/f",
		}),
		ReadFile: fileMap(map[string]string{"/f": "fromfile\n"}),
	}
	got, err := r.Resolve("oidc_client_secret")
	if err == nil {
		t.Fatalf("plain secret env accepted next to _FILE: %+v", got)
	}
	msg := err.Error()
	if !strings.Contains(msg, "MINION_OIDC_CLIENT_SECRET is not accepted") {
		t.Errorf("error %q does not name MINION_OIDC_CLIENT_SECRET", msg)
	}
	if strings.Contains(msg, secret) || strings.Contains(msg, "fromfile") {
		t.Fatalf("error leaks a value: %q", msg)
	}
	// An unreadable _FILE does not turn the rejection into a different error.
	r.ReadFile = fileMap(nil)
	if _, err := r.Resolve("oidc_client_secret"); err == nil || !strings.Contains(err.Error(), "is not accepted") {
		t.Errorf("with unreadable _FILE: %v", err)
	}
}

func TestResolvedSecretNeverFormatted(t *testing.T) {
	const secret = "s3cret-value-xyz"
	r := &Resolver{
		Registry:  hubRegistry(),
		Role:      ScopeHub,
		LookupEnv: envMap(nil),
		Panel:     MapSource{"oidc_client_secret": secret},
	}
	res, err := r.Resolve("oidc_client_secret")
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{
		"%#v":    fmt.Sprintf("%#v", res),
		"%v":     fmt.Sprintf("%v", res),
		"Sprint": fmt.Sprint(res),
	} {
		if strings.Contains(out, secret) || !strings.Contains(out, Redacted) {
			t.Errorf("%s output %q", name, out)
		}
	}
	for name, mk := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(mk(&buf)).Info("resolved", "setting", res)
		out := buf.String()
		if strings.Contains(out, secret) || !strings.Contains(out, Redacted) || strings.Contains(out, "ERROR") {
			t.Errorf("slog %s output %q", name, out)
		}
	}
	// A non-secret still logs its value.
	var buf bytes.Buffer
	plain := Resolved{Setting: Setting{Key: "k"}, Value: "visible", Source: SourceDefault}
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "setting", plain)
	if !strings.Contains(buf.String(), "visible") {
		t.Errorf("non-secret value missing: %q", buf.String())
	}
}

func TestSecretChoicesErrorDoesNotEchoValue(t *testing.T) {
	const secret = "bad-secret-choice"
	reg := NewRegistry()
	reg.Declare(Setting{Key: "tok", Scope: ScopeAny, Secret: true, PlainEnvAllowed: true, Choices: []string{"a", "b"}})
	r := &Resolver{Registry: reg, Role: ScopeWorker, LookupEnv: envMap(map[string]string{"MINION_TOK": secret})}
	_, err := r.Resolve("tok")
	if err == nil {
		t.Fatal("bad choice accepted")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), Redacted) {
		t.Fatalf("error %q", err)
	}
}
