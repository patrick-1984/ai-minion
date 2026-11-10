package config

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testInviteKey = "mnk_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst.abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"

func inviteResolver(flags MapSource, env map[string]string, file FileSource) *Resolver {
	r := &Resolver{Role: ScopeWorker, LookupEnv: envMap(env), File: file}
	if flags != nil {
		r.Flags = flags
	}
	return r
}

func TestInviteKeyIsDeclaredWithFlagAndFileEnvironment(t *testing.T) {
	s, ok := Default.Lookup(KeyInviteKey)
	if !ok {
		t.Fatal("invite_key is not declared")
	}
	if !s.Secret || !s.ArgvAllowedWithWarning || !s.HasFlag() || s.Scope != ScopeWorker {
		t.Fatalf("setting = %+v", s)
	}
	if s.FlagName() != "invite-key" || s.FileEnvName() != "MINION_INVITE_KEY_FILE" {
		t.Fatalf("flag --%s, file env %s", s.FlagName(), s.FileEnvName())
	}
}

func TestClassifyInviteKeyArg(t *testing.T) {
	cases := map[string]InviteKeyForm{
		testInviteKey:         InviteKeyLiteral,
		"mnk_":                InviteKeyLiteral,
		"-":                   InviteKeyStdin,
		"/run/secrets/invite": InviteKeyPath,
		"invite.txt":          InviteKeyPath,
		"./mnk_file":          InviteKeyPath,
		"--":                  InviteKeyPath,
		"":                    InviteKeyPath,
	}
	for arg, want := range cases {
		if got := ClassifyInviteKeyArg(arg); got != want {
			t.Errorf("ClassifyInviteKeyArg(%q) = %s, want %s", arg, got, want)
		}
	}
}

func TestLiteralInviteKeyOnTheCommandLineWarnsWithTheExactLine(t *testing.T) {
	in, err := ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: testInviteKey}, nil, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Set || in.Value != testInviteKey || in.Form != InviteKeyLiteral || in.Source != SourceFlag {
		t.Fatalf("input = %v", in)
	}
	const golden = "warning: the invitation key given on the command line is visible in the process list; " +
		"prefer --invite-key <file> or - (stdin)"
	if in.Warning != golden || InviteKeyArgvWarning != golden {
		t.Fatalf("warning = %q", in.Warning)
	}
	if strings.Contains(in.Warning, "mnk_") || strings.Contains(in.Warning, testInviteKey) {
		t.Fatalf("the warning repeats the key: %q", in.Warning)
	}
}

func TestInviteKeyFromAFileAndStdinHasNoWarningAndTakesTheFirstLineTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite")
	if err := os.WriteFile(path, []byte("  "+testInviteKey+"  \r\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		flag  string
		stdin string
		form  InviteKeyForm
	}{
		{"file", path, "", InviteKeyPath},
		{"stdin", "-", testInviteKey + "\nignored\n", InviteKeyStdin},
		{"stdin without newline", "-", "  " + testInviteKey, InviteKeyStdin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, err := ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: c.flag}, nil, nil), strings.NewReader(c.stdin))
			if err != nil {
				t.Fatal(err)
			}
			if in.Value != testInviteKey || in.Form != c.form || in.Warning != "" {
				t.Fatalf("input = %v warning %q value-ok=%v", in, in.Warning, in.Value == testInviteKey)
			}
		})
	}
}

func TestInviteKeyEnvironmentFormIsTheFileVariable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite")
	if err := os.WriteFile(path, []byte(testInviteKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := ResolveWorkerInviteKey(inviteResolver(nil, map[string]string{"MINION_INVITE_KEY_FILE": path}, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Value != testInviteKey || in.Source != SourceEnv || in.Form != InviteKeyPath || in.Warning != "" {
		t.Fatalf("input = %v", in)
	}

	// The plain variable is a secret in the environment: refused, never echoed.
	_, err = ResolveWorkerInviteKey(inviteResolver(nil, map[string]string{"MINION_INVITE_KEY": testInviteKey}, nil), nil)
	if err == nil || strings.Contains(err.Error(), testInviteKey) || !strings.Contains(err.Error(), "MINION_INVITE_KEY_FILE") {
		t.Fatalf("plain env error = %v", err)
	}

	// The flag wins over the environment.
	in, err = ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: testInviteKey},
		map[string]string{"MINION_INVITE_KEY_FILE": path}, nil), nil)
	if err != nil || in.Source != SourceFlag {
		t.Fatalf("flag over env: %v, %v", in, err)
	}
}

func TestInviteKeyFromTheConfigFileIsLiteralWithoutWarning(t *testing.T) {
	in, err := ResolveWorkerInviteKey(inviteResolver(nil, nil, MapSource{KeyInviteKey: testInviteKey}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Value != testInviteKey || in.Source != SourceFile || in.Warning != "" {
		t.Fatalf("input = %v", in)
	}
}

func TestNoInviteKeyResolvesToUnset(t *testing.T) {
	in, err := ResolveWorkerInviteKey(inviteResolver(nil, nil, nil), nil)
	if err != nil || in.Set || in.Value != "" || in.Warning != "" {
		t.Fatalf("input = %v, err %v", in, err)
	}
}

func TestInviteKeyErrorsNeverEchoAKeyLikeValue(t *testing.T) {
	// A key typed without its prefix is treated as a path; the error must not
	// repeat it.
	typo := strings.TrimPrefix(testInviteKey, "mnk_")
	_, err := ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: typo}, nil, nil), nil)
	if err == nil {
		t.Fatal("a missing file was accepted")
	}
	if strings.Contains(err.Error(), typo) || strings.Contains(err.Error(), "abcdefghijklmnop") {
		t.Fatalf("the error repeats the value: %v", err)
	}
	if !strings.Contains(err.Error(), "mnk_") {
		t.Fatalf("the error does not explain the form: %v", err)
	}

	// A real path is named.
	missing := filepath.Join(t.TempDir(), "nope")
	_, err = ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: missing}, nil, nil), nil)
	if err == nil || !strings.Contains(err.Error(), missing) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v", err)
	}
}

func TestInviteKeyEmptyInputsAreErrors(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"empty file": func() error {
			_, err := ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: empty}, nil, nil), nil)
			return err
		},
		"empty stdin": func() error {
			_, err := ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: "-"}, nil, nil), strings.NewReader(""))
			return err
		},
		"no stdin": func() error {
			_, err := ResolveWorkerInviteKey(inviteResolver(MapSource{KeyInviteKey: "-"}, nil, nil), nil)
			return err
		},
	} {
		if err := run(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestInviteKeyInputNeverPrintsTheKey(t *testing.T) {
	in := InviteKeyInput{Set: true, Value: testInviteKey, Source: SourceFlag, Form: InviteKeyLiteral}
	for _, s := range []string{in.String(), in.GoString()} {
		if strings.Contains(s, "abcdefghijklmnop") {
			t.Fatalf("printed the key: %s", s)
		}
	}
}

func TestInviteKeyGivenReadsNoFileAndNoStdin(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	cases := []struct {
		name           string
		res            *Resolver
		given, literal bool
	}{
		{"nothing", inviteResolver(nil, nil, nil), false, false},
		{"literal flag", inviteResolver(MapSource{KeyInviteKey: testInviteKey}, nil, nil), true, true},
		{"stdin flag", inviteResolver(MapSource{KeyInviteKey: "-"}, nil, nil), true, false},
		{"path flag that does not exist", inviteResolver(MapSource{KeyInviteKey: missing}, nil, nil), true, false},
		{"empty flag", inviteResolver(MapSource{KeyInviteKey: ""}, map[string]string{"MINION_INVITE_KEY_FILE": missing}, nil), false, false},
		{"file env that does not exist", inviteResolver(nil, map[string]string{"MINION_INVITE_KEY_FILE": missing}, nil), true, false},
		{"config file", inviteResolver(nil, nil, MapSource{KeyInviteKey: testInviteKey}), true, false},
	}
	for _, c := range cases {
		given, literal, err := InviteKeyGiven(c.res)
		if err != nil || given != c.given || literal != c.literal {
			t.Errorf("%s: given %v literal %v err %v, want %v %v", c.name, given, literal, err, c.given, c.literal)
		}
	}
}

func TestInviteKeyFromStdinStopsWhenTheContextEnds(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ResolveWorkerInviteKeyContext(ctx, inviteResolver(MapSource{KeyInviteKey: "-"}, nil, nil), pr)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("returned before the cancel: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a pending stdin read ignored the cancel")
	}
}
