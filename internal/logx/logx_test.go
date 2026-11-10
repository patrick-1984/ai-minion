package logx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func forceTTY(v bool) func(io.Writer) bool { return func(io.Writer) bool { return v } }

func TestAutoFormatFollowsTTY(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var buf bytes.Buffer
		l, err := New(Options{Output: &buf, IsTerminal: forceTTY(tty)})
		if err != nil {
			t.Fatal(err)
		}
		l.Info("hello", "k", "v")
		out := buf.String()
		isJSON := json.Valid([]byte(strings.TrimSpace(out)))
		if tty && (isJSON || !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v")) {
			t.Errorf("tty: expected text, got %q", out)
		}
		if !tty && !isJSON {
			t.Errorf("non-tty: expected JSON, got %q", out)
		}
	}
}

func TestFormatOverride(t *testing.T) {
	var buf bytes.Buffer
	l, _ := New(Options{Output: &buf, Format: FormatJSON, IsTerminal: forceTTY(true)})
	l.Info("x")
	if !json.Valid(buf.Bytes()) {
		t.Errorf("json override on a tty: %q", buf.String())
	}
	buf.Reset()
	l, _ = New(Options{Output: &buf, Format: "TEXT", IsTerminal: forceTTY(false)})
	l.Info("x")
	if json.Valid(buf.Bytes()) {
		t.Errorf("text override off a tty: %q", buf.String())
	}
}

func TestSecretRedactedInBothFormats(t *testing.T) {
	const raw = "super-secret-value"
	for _, format := range []Format{FormatJSON, FormatText} {
		t.Run(string(format), func(t *testing.T) {
			var buf bytes.Buffer
			l, err := New(Options{Output: &buf, Format: format})
			if err != nil {
				t.Fatal(err)
			}
			l.Info("login",
				"password", Secret(raw),
				slog.Any("key", Secret(raw)),
				slog.Group("g", slog.Any("nested", Secret(raw))),
			)
			l.With("bound", Secret(raw)).Warn("with")
			out := buf.String()
			if strings.Contains(out, raw) {
				t.Fatalf("secret leaked: %s", out)
			}
			if strings.Count(out, "[redacted]") != 4 {
				t.Fatalf("want 4 redactions, got output: %s", out)
			}
		})
	}
}

func TestJSONRecordShape(t *testing.T) {
	var buf bytes.Buffer
	l, _ := New(Options{Output: &buf, Format: FormatJSON})
	l.Info("m", "password", Secret("x"))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["password"] != "[redacted]" || rec["msg"] != "m" || rec["level"] != "INFO" {
		t.Fatalf("record = %v", rec)
	}
}

func TestSecretOtherOutputPaths(t *testing.T) {
	s := Secret("topsecret")
	for name, got := range map[string]string{
		"%s":     fmt.Sprintf("%s", s),
		"%v":     fmt.Sprintf("%v", s),
		"%q":     fmt.Sprintf("%q", s),
		"%x":     fmt.Sprintf("%x", s),
		"%+v":    fmt.Sprintf("%+v", s),
		"%#v":    fmt.Sprintf("%#v", s),
		"%d":     fmt.Sprintf("%d", s),
		"struct": fmt.Sprintf("%+v", struct{ P Secret }{s}),
		"ptr":    fmt.Sprintf("%v", &s),
		"print":  fmt.Sprint(s),
		"String": s.String(),
	} {
		if strings.Contains(got, "topsecret") || !strings.Contains(got, "[redacted]") {
			t.Errorf("%s printed %q", name, got)
		}
	}
	j, err := json.Marshal(map[string]any{"p": s, "q": struct{ P Secret }{s}})
	if err != nil || strings.Contains(string(j), "topsecret") {
		t.Errorf("json: %s %v", j, err)
	}
	if s.Reveal() != "topsecret" {
		t.Error("Reveal")
	}
	if v := s.LogValue(); v.String() != "[redacted]" {
		t.Errorf("LogValue = %v", v)
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(Options{Output: &buf, Format: FormatText, Level: "WARN"})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("quiet")
	l.Warn("loud")
	if out := buf.String(); strings.Contains(out, "quiet") || !strings.Contains(out, "loud") {
		t.Fatalf("output = %q", out)
	}
	buf.Reset()
	l, _ = New(Options{Output: &buf, Format: FormatText, Level: "debug"})
	l.Debug("dbg")
	if !strings.Contains(buf.String(), "dbg") {
		t.Fatal("debug dropped")
	}
}

func TestInvalidOptions(t *testing.T) {
	if _, err := New(Options{Level: "loud"}); err == nil || !strings.Contains(err.Error(), "invalid log level") {
		t.Errorf("level: %v", err)
	}
	if _, err := New(Options{Format: "xml"}); err == nil || !strings.Contains(err.Error(), "invalid log format") {
		t.Errorf("format: %v", err)
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "info": slog.LevelInfo, " Debug ": slog.LevelDebug, "warning": slog.LevelWarn, "warn": slog.LevelWarn, "ERROR": slog.LevelError} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
	for in, want := range map[string]Format{"": FormatAuto, "auto": FormatAuto, "JSON": FormatJSON, "text": FormatText} {
		if got, err := ParseFormat(in); err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v", in, got, err)
		}
	}
}

func TestDefaultOutputIsStderrAndIsTerminal(t *testing.T) {
	if IsTerminal(&bytes.Buffer{}) {
		t.Error("buffer is not a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("regular file is not a terminal")
	}
	if null, err := os.Open(os.DevNull); err == nil {
		defer null.Close()
		if IsTerminal(null) {
			t.Error("os.DevNull is not a terminal")
		}
	}
	// New with no options must not fail and must default to stderr.
	if _, err := New(Options{}); err != nil {
		t.Fatal(err)
	}
}
