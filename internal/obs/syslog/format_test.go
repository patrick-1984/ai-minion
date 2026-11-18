package syslog

import (
	"bytes"
	"encoding/json"
	"flag"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var goldenTime = time.Date(2026, 11, 18, 20, 48, 1, 123456789, time.FixedZone("CET", 3600))

func fields(kv ...any) []Field {
	var as []slog.Attr
	for i := 0; i+1 < len(kv); i += 2 {
		as = append(as, slog.Any(kv[i].(string), kv[i+1]))
	}
	return Fields(as)
}

func TestGoldenLines(t *testing.T) {
	f := &Formatter{Hostname: "hub-1.example", AppName: "minion-hub", ProcID: "4242", MaxBytes: 2048}
	cases := []struct {
		name  string
		level slog.Level
		msgID string
		msg   string
		fs    []Field
		max   int
	}{
		{"info", slog.LevelInfo, "workers", "worker connected", fields("worker_id", "w-1", "n", 3, "ok", true), 0},
		{"debug", slog.LevelDebug, "", "op routed", fields("duration", 1500*time.Millisecond), 0},
		{"warn", slog.LevelWarn, "bad msgid", "unicode ✓ <tag> & \"quotes\"", fields("g", slog.GroupValue(slog.String("a", "b"))), 0},
		{"error", slog.LevelError, "x", "failed", fields("error", "boom"), 0},
		{"truncated", slog.LevelInfo, "big", "keeps the message", fields("small", "s", "large", strings.Repeat("L", 600), "mid", strings.Repeat("M", 100)), 480},
		{"cut-message", slog.LevelInfo, "-", strings.Repeat("é", 400), nil, 480},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ff := *f
			if c.max != 0 {
				ff.MaxBytes = c.max
			}
			got := ff.Format(goldenTime, c.level, c.msgID, c.msg, c.fs)
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got\n%q\nwant\n%q", got, want)
			}
			checkMessage(t, got, ff.MaxBytes)
		})
	}
}

// checkMessage asserts the invariants: within budget, valid UTF-8, a BOM
// right after the header and one valid JSON object after it.
func checkMessage(t *testing.T, m []byte, max int) {
	t.Helper()
	if len(m) > max {
		t.Fatalf("message of %d bytes exceeds %d", len(m), max)
	}
	if !utf8.Valid(m) {
		t.Fatalf("invalid UTF-8: %q", m)
	}
	i := bytes.Index(m, bom)
	if i < 0 {
		t.Fatalf("no BOM: %q", m)
	}
	if !bytes.HasSuffix(m[:i], []byte(" - ")) {
		t.Fatalf("the BOM does not follow the NILVALUE structured data: %q", m[:i])
	}
	body := m[i+len(bom):]
	var obj map[string]any
	if !json.Valid(body) || json.Unmarshal(body, &obj) != nil {
		t.Fatalf("MSG is not a JSON object: %q", body)
	}
}

func TestHeaderFields(t *testing.T) {
	f := &Formatter{Hostname: "host with space", AppName: strings.Repeat("a", 49), ProcID: "1", MaxBytes: 2048}
	m := string(f.Format(goldenTime, slog.LevelError, "comp\x01", "x", nil))
	if !strings.HasPrefix(m, "<131>1 2026-11-18T20:48:01.123456+01:00 - - 1 - - \xEF\xBB\xBF{") {
		t.Fatalf("header = %q", m)
	}
	for in, want := range map[string]string{"": "-", "ok": "ok", "é": "-", "a b": "-", strings.Repeat("x", 33): "-", strings.Repeat("x", 32): strings.Repeat("x", 32)} {
		if got := HeaderField(in, maxMsgID); got != want {
			t.Errorf("HeaderField(%q) = %q", in, got)
		}
	}
	for l, pri := range map[slog.Level]string{slog.LevelDebug: "<135>", slog.LevelInfo: "<134>", slog.LevelWarn: "<132>", slog.LevelError: "<131>"} {
		if m := f.Format(goldenTime, l, "", "x", nil); !bytes.HasPrefix(m, []byte(pri)) {
			t.Errorf("level %v: %q", l, m[:6])
		}
	}
}

func TestOctetFrame(t *testing.T) {
	if got := string(OctetFrame([]byte("<134>1 é"))); got != "9 <134>1 é" {
		t.Fatalf("frame = %q", got)
	}
}

// Random records of every size always yield a valid message within budget.
func TestReductionRandomRecords(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	alphabet := []rune("aZ09 \"\\\n\t<>&é€😀\x00\x7f")
	str := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteRune(alphabet[r.IntN(len(alphabet))])
		}
		return b.String()
	}
	for i := range 2000 {
		max := 480 + r.IntN(4000)
		f := &Formatter{Hostname: str(r.IntN(300)), AppName: "minion-worker", ProcID: "7", MaxBytes: max}
		var as []slog.Attr
		for j := range r.IntN(12) {
			as = append(as, slog.String(str(1+r.IntN(10))+string(rune('a'+j)), str(r.IntN(1500))))
		}
		m := f.Format(time.Now(), slog.Level(r.IntN(16)-4), str(r.IntN(40)), str(r.IntN(5000)), Fields(as))
		checkMessage(t, m, max)
		if t.Failed() {
			t.Fatalf("iteration %d", i)
		}
	}
}

func FuzzFormat(f *testing.F) {
	f.Add("msg", "k", "v", 480)
	f.Add(strings.Repeat("é", 300), "kk", strings.Repeat("\x00", 900), 2048)
	f.Fuzz(func(t *testing.T, msg, k, v string, max int) {
		if max < 480 || max > 65000 {
			max = 480 + (max%1000+1000)%1000
		}
		fm := &Formatter{Hostname: "h", AppName: "a", ProcID: "1", MaxBytes: max}
		checkMessage(t, fm.Format(goldenTime, slog.LevelInfo, k, msg, Fields([]slog.Attr{slog.String(k, v)})), max)
	})
}
