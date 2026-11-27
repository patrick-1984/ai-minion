package devctl

import (
	"errors"
	"strings"
	"testing"
)

func TestShellCommand(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"ls | wc -l"}, "ls | wc -l"},
		{[]string{"sh", "-c", "exit 7"}, "sh -c 'exit 7'"},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"id", "-u"}, "id -u"},
		{[]string{"echo", ""}, "echo ''"},
	}
	for _, c := range cases {
		got, err := ShellCommand(c.argv)
		if err != nil || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.argv, got, err, c.want)
		}
	}
	for _, bad := range [][]string{nil, {"  "}, {"a\x00b"}} {
		if _, err := ShellCommand(bad); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestInputCommand(t *testing.T) {
	ok := map[string][]string{
		"input keyevent KEYCODE_HOME":    {"keyevent", "KEYCODE_HOME"},
		"input keyevent 3 4":             {"keyevent", "3", "4"},
		"input text hello%sworld":        {"text", "hello world"},
		"input text abc":                 {"text", "abc"},
		"input tap 10 20":                {"tap", "10", "20"},
		"input swipe 1 2 3 4":            {"swipe", "1", "2", "3", "4"},
		"input swipe 1 2 3 4 300":        {"swipe", "1", "2", "3", "4", "300"},
		"input text 'a;b'":               {"text", "a;b"},
		`input text 'it'\''s'`:           {"text", "it's"},
		"input keyevent KEYCODE_DPAD_UP": {"keyevent", "KEYCODE_DPAD_UP"},
	}
	for want, argv := range ok {
		got, err := InputCommand(argv)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", argv, got, err, want)
		}
	}
	bad := [][]string{
		nil, {"keyevent"}, {"keyevent", "HOME; reboot"}, {"keyevent", "keycode_home"},
		{"text"}, {"text", ""}, {"text", "a", "b"}, {"text", "tab\there"}, {"text", "caf\xc3\xa9"},
		{"tap", "1"}, {"tap", "-1", "2"}, {"tap", "1", "2x"}, {"tap", "1", "99999"}, {"tap", "01", "2"},
		{"swipe", "1", "2", "3"}, {"swipe", "1", "2", "3", "4", "20000"},
		{"roll", "1"}, {"draganddrop", "1", "2", "3", "4"},
	}
	for _, argv := range bad {
		if _, err := InputCommand(argv); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%q accepted", argv)
		}
	}
}

func TestParseStatPPID(t *testing.T) {
	n, err := parseStatPPID([]byte("1234 (sh (x) y) S 77 1234 1234 0 -1"))
	if err != nil || n != 77 {
		t.Fatal(n, err)
	}
	if _, err := parseStatPPID([]byte("garbage")); err == nil {
		t.Fatal("accepted")
	}
}

func TestTokenLine(t *testing.T) {
	var tok Token
	tok.ReadLine(strings.NewReader("MINION_ADB_BRIDGE_TOKEN=" + strings.Repeat("ab", 32) + "\nrest"))
	if tok.Get() != strings.Repeat("ab", 32) {
		t.Fatal(tok.Get())
	}
	for _, bad := range []string{"", "MINION_ADB_BRIDGE_TOKEN=short\n", "OTHER=" + strings.Repeat("ab", 32) + "\n"} {
		var b Token
		b.ReadLine(strings.NewReader(bad))
		if b.Get() != "" {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestTokenLineLeavesTheRestOfStdin(t *testing.T) {
	r := strings.NewReader("MINION_ADB_BRIDGE_TOKEN=" + strings.Repeat("cd", 32) + "\nnext line\n")
	var tok Token
	tok.ReadLine(r)
	rest := make([]byte, 64)
	n, _ := r.Read(rest)
	if tok.Get() == "" || string(rest[:n]) != "next line\n" {
		t.Fatalf("token %q rest %q", tok.Get(), rest[:n])
	}
}
