package smoke

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// These tests run macos.sh against a fake "minion" (a bash script in the
// archive) and a stub tmux, so that every way a Worker can fail to start is
// reproduced on purpose and without a real tmux server. They guard the
// failure diagnostics: a Worker that dies, hangs or vanishes must never end
// the script with a bare "exit 1" (CI runs 129830135263 and 129833120052 did).

const (
	fakeSecret = "hunter2-top-secret-value"
	fakeToken  = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	// A bearer credential shorter than the 32 characters of the long-run rule.
	fakeShort = "s3cr3tb"
)

// fakeMinion is the body of the fake binary. FAKE_RUN selects what "worker
// run" does, FAKE_STATUS what "worker status" does.
const fakeMinion = `#!/usr/bin/env bash
pad() { local i o=""; for ((i = 0; i < $1; i++)); do o+="a "; done; printf '%s' "$o"; }
sock=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
	[ "${args[i]}" = "--tmux-socket" ] && sock=${args[i+1]}
done
case "$1 ${2:-}" in
"version ")
	printf 'version: v0.1.0\ncommit: fake\n'
	;;
"worker run")
	echo "level=INFO msg=starting" >&2
	echo "level=ERROR msg=\"cannot start\" password=` + fakeSecret + ` api_token=` + fakeToken + ` Authorization: Bearer ` + fakeToken + ` exit code: 3" >&2
	echo "Authorization: Bearer ` + fakeShort + `" >&2
	# The cut at 400 characters falls inside the quoted value (380 + 10 + 10).
	echo "$(pad 190)password=\"` + fakeSecret + `\"" >&2
	case "${FAKE_RUN:-die}" in
	die) exit 3 ;;
	live) exec sleep 60 ;;
	dies-late) sleep 1; exit 5 ;;
	die-slow) sleep 1.5; : >"$XDG_RUNTIME_DIR/../died"; exit 3 ;;
	esac
	;;
"worker status")
	case "${FAKE_STATUS:-ok}" in
	never)
		echo "Connection: unknown"
		# The cut at 200 characters (timeout message) and at 400 (report).
		echo "Problem: $(pad 85)password=\"` + fakeSecret + `\""
		echo "Problem: $(pad 190)password=\"` + fakeSecret + `\""
		echo "Problem: token=` + fakeSecret + ` could not reach the Worker"
		exit 2
		;;
	slow) sleep 2 ;;
	spawn)
		# Before the Worker is dead this is "never"; afterwards (the failure
		# report) it starts descendants (a child and a grandchild, like "worker
		# status" starting a tmux client) and then waits for them for good.
		if [ ! -e "$XDG_RUNTIME_DIR/../died" ]; then
			echo "Connection: unknown"
			exit 2
		fi
		bash -c 'sleep 40 & echo $! >>"$1"; wait' _ "$FAKE_PIDS" &
		sleep 40 &
		echo $! >>"$FAKE_PIDS"
		wait
		;;
	esac
	# The first line already satisfies "grep -q": the rest must still be
	# written without a SIGPIPE (a status longer than a pipe buffer).
	echo "Connection: idle (not enrolled)"
	for ((n = 0; n < 4000; n++)); do echo "Padding: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx $n"; done
	echo "Version:  v0.1.0"
	echo "Sessions: 1"
	echo "tmux socket:  $sock"
	;;
esac
`

// fakeTmux answers the read-only calls of the failure report.
const fakeTmux = `case "${STUB_TMUX:-}" in
stall) exec sleep 20 ;;
stallchild) sleep 20; exit 0 ;;
esac
case "$*" in
*-V*) echo "tmux 3.8" ;;
*) echo "no server running on the fake socket" >&2; exit 1 ;;
esac
`

// runFakeMacos runs macos.sh with the fake binary; extra is the environment of
// the fake ("FAKE_RUN=...") and of the script. It returns the result and the
// short TMPDIR the script worked in.
func runFakeMacos(t *testing.T, extra ...string) (result, string) {
	t.Helper()
	needTools(t, "bash")
	tmp := shortTemp(t)
	// An existing private socket that is not a tmux server: the script only
	// points the Worker at it.
	sockPath := filepath.Join(tmp, "f.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	env, _ := stubBin(t, map[string]string{"tmux": fakeTmux})
	return runFakeMacosOn(t, tmp, sockPath, env, extra...), tmp
}

// runFakeMacosOn runs macos.sh against the private socket sockPath (a tmux
// server of the caller) with env (for example a stub tmux first on PATH).
func runFakeMacosOn(t *testing.T, tmp, sockPath string, env []string, extra ...string) result {
	t.Helper()
	env = append(env, "SMOKE_OS=linux", "SMOKE_ARCH=amd64", "TMPDIR="+tmp, "SMOKE_TMUX_SOCKET="+sockPath)
	env = append(env, extra...)
	assets := fixtureAssets(t, map[string]string{"minion_0.1.0_linux_amd64.tar.gz": tarGz(t, "minion", []byte(fakeMinion))})
	return script(t, env, "", "macos.sh", assets, testTag)
}

func leftovers(t *testing.T, tmp string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(tmp, "mn.*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustHave(t *testing.T, r result, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(r.all(), s) {
			t.Errorf("the output lacks %q:\n%s", s, r.all())
		}
	}
}

func mustNotLeak(t *testing.T, r result) {
	t.Helper()
	for _, s := range []string{fakeSecret, "hunter2", fakeToken, fakeShort} {
		if strings.Contains(r.all(), s) {
			t.Errorf("the output leaks a secret value %q:\n%s", s, r.all())
		}
	}
}

// A Worker that exits at once is the failure of the CI runs: the script must
// say so, with the exit status and the (redacted) end of the Worker log, and
// must still remove its work directory.
func TestMacosSmokeReportsAWorkerThatExitsAtStart(t *testing.T) {
	r, tmp := runFakeMacos(t, "FAKE_RUN=die", "FAKE_STATUS=never")
	if r.code == 0 {
		t.Fatalf("a Worker that exited passed: %s", r.all())
	}
	mustHave(t, r,
		"version: v0.1.0",
		"FAIL: the Worker exited with status 3",
		"--- failure report",
		"the Worker (pid ",
		"exited with status 3",
		"tmux 3.8",
		"last lines of the Worker log (redacted):",
		"msg=\"cannot start\"",
		"exit code: 3", // not a secret: must stay readable
		"[redacted]",
		"minion worker status (redacted):",
	)
	mustNotLeak(t, r)
	if l := leftovers(t, tmp); len(l) != 0 {
		t.Errorf("the work directory was not removed: %v", l)
	}
}

// A Worker that runs but is never reachable ends in a timeout that says what
// the last status was; the Worker log is reported before it is deleted.
func TestMacosSmokeReportsAStatusThatNeverReachesTheWorker(t *testing.T) {
	r, tmp := runFakeMacos(t, "FAKE_RUN=live", "FAKE_STATUS=never", "SMOKE_WORKER_WAIT_SECONDS=2")
	if r.code == 0 {
		t.Fatalf("an unreachable Worker passed: %s", r.all())
	}
	mustHave(t, r,
		"FAIL: timed out after 2s waiting for minion worker status",
		"last status (exit 2)",
		"Connection: unknown",
		"is still running",
		"last lines of the Worker log (redacted):",
		"msg=\"cannot start\"",
	)
	mustNotLeak(t, r)
	if l := leftovers(t, tmp); len(l) != 0 {
		t.Errorf("the work directory was not removed: %v", l)
	}
}

// A Worker that is gone when the checks are over used to end the script with
// a silent exit 1 (set -e on "kill"): it must be reported.
func TestMacosSmokeReportsAWorkerThatVanishesAfterTheChecks(t *testing.T) {
	r, tmp := runFakeMacos(t, "FAKE_RUN=dies-late", "FAKE_STATUS=slow")
	if r.code == 0 {
		t.Fatalf("a Worker that vanished passed: %s", r.all())
	}
	mustHave(t, r,
		"FAIL: the Worker was gone before the smoke test ended",
		"--- failure report",
		"exited with status 5",
	)
	mustNotLeak(t, r)
	if l := leftovers(t, tmp); len(l) != 0 {
		t.Errorf("the work directory was not removed: %v", l)
	}
}

// The happy path with a status that is much longer than a pipe buffer and
// whose first line already matches: "worker status | grep -q" under pipefail
// ends in SIGPIPE here, the script must not.
func TestMacosSmokeSurvivesALongStatusUnderPipefail(t *testing.T) {
	r, tmp := runFakeMacos(t, "FAKE_RUN=live")
	if r.code != 0 {
		t.Fatalf("macos.sh: exit %d %s", r.code, r.all())
	}
	mustHave(t, r, "macos smoke test passed")
	if strings.Contains(r.all(), "failure report") {
		t.Errorf("a passing run printed a failure report: %s", r.all())
	}
	if l := leftovers(t, tmp); len(l) != 0 {
		t.Errorf("the work directory was not removed: %v", l)
	}
}

func TestSmokeRedact(t *testing.T) {
	in := strings.Join([]string{
		`level=ERROR password=` + fakeSecret + ` user=anna`,
		`{"token":"` + fakeSecret + `","state":"ok"}`,
		`Authorization: Bearer ` + fakeToken,
		`Pairing code: ABCD-EFGH`,
		`api_key: ` + fakeSecret,
		`blob ` + fakeToken + ` end`,
		`exit status 3, pid 1234, socket /tmp/sm1/mn.x/t.sock`,
		`Authorization: Bearer ` + fakeShort,
		`bearer ` + fakeShort + ` and more`,
		`cut password="` + fakeSecret[:12], // a quote that is never closed
		`cut token='` + fakeSecret[:12],
	}, "\n")
	r := sh(t, []string{"IN=" + in}, `printf '%s\n' "$IN" | smoke_redact`)
	if r.code != 0 {
		t.Fatalf("smoke_redact: %s", r.all())
	}
	for _, leak := range []string{fakeSecret, fakeToken, "ABCD-EFGH", fakeShort, "hunter2"} {
		if strings.Contains(r.stdout, leak) {
			t.Errorf("smoke_redact leaks %q:\n%s", leak, r.stdout)
		}
	}
	for _, keep := range []string{"user=anna", `"state":"ok"`, "exit status 3, pid 1234", "/tmp/sm1/mn.x/t.sock"} {
		if !strings.Contains(r.stdout, keep) {
			t.Errorf("smoke_redact removed %q:\n%s", keep, r.stdout)
		}
	}
}

// The poll of macos.sh must not go through smoke_wait_for again: it sends the
// output of a smoke_die inside it to /dev/null.
func TestMacosScriptDoesNotHideItsPoll(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "test", "smoke", "macos.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^\s*smoke_wait_for\s`).Match(src) {
		t.Error("macos.sh polls through smoke_wait_for, which hides what a smoke_die inside the poll says")
	}
}

// A secret that crosses a truncation width (400 characters in the log and the
// report, 200 in the timeout message) is redacted whole: redaction comes
// before truncation on every path (log, status, early exit, timeout).
func TestMacosSmokeRedactsBeforeItTruncates(t *testing.T) {
	r, _ := runFakeMacos(t, "FAKE_RUN=die", "FAKE_STATUS=never")
	if r.code == 0 {
		t.Fatalf("a Worker that exited passed: %s", r.all())
	}
	mustHave(t, r, "FAIL: the Worker exited with status 3", "last lines of the Worker log (redacted):")
	mustNotLeak(t, r)

	r, _ = runFakeMacos(t, "FAKE_RUN=live", "FAKE_STATUS=never", "SMOKE_WORKER_WAIT_SECONDS=1")
	if r.code == 0 {
		t.Fatalf("an unreachable Worker passed: %s", r.all())
	}
	mustHave(t, r, "FAIL: timed out after 1s", "minion worker status (redacted):")
	mustNotLeak(t, r)
}

// A tmux that never answers (a stopped server of the caller) must not keep the
// script from reporting, stopping the Worker and removing its directory; the
// status of the underlying failure stays. The second mode leaves a child
// process behind that outlives the tmux the script gave up on.
func TestMacosSmokeCleansUpWhenTmuxStalls(t *testing.T) {
	for _, mode := range []string{"stall", "stallchild"} {
		t.Run(mode, func(t *testing.T) {
			start := time.Now()
			r, tmp := runFakeMacos(t, "FAKE_RUN=die", "FAKE_STATUS=never", "STUB_TMUX="+mode, "SMOKE_DIAG_TIMEOUT_SECONDS=1")
			if el := time.Since(start); el > 30*time.Second {
				t.Errorf("the failure report took %v with a tmux that never answers", el)
			}
			if r.code != 1 {
				t.Errorf("exit %d, want the exit status 1 of the failure: %s", r.code, r.all())
			}
			mustHave(t, r,
				"FAIL: the Worker exited with status 3", // the underlying failure, not the diagnostics
				"(stopped after 1s)",
				"last lines of the Worker log (redacted):",
				"msg=\"cannot start\"",
				"minion worker status (redacted):",
			)
			mustNotLeak(t, r)
			if l := leftovers(t, tmp); len(l) != 0 {
				t.Errorf("the work directory was not removed: %v", l)
			}
		})
	}
}

func TestSmokeTimeout(t *testing.T) {
	start := time.Now()
	r := sh(t, nil, `out=$(smoke_timeout 1 sleep 30) && rc=0 || rc=$?; printf 'rc=%s out=[%s]\n' "$rc" "$out"`)
	if r.code != 0 || !strings.Contains(r.stdout, "rc=124 out=[]") {
		t.Errorf("a command that outlives the limit: exit %d %s", r.code, r.all())
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("smoke_timeout took %v for a 1 second limit", el)
	}

	// The output and the exit status of a command that ends in time.
	r = sh(t, nil, `out=$(smoke_timeout 5 bash -c 'echo out; echo err >&2; exit 7') && rc=0 || rc=$?; printf 'rc=%s out=[%s]\n' "$rc" "$(printf '%s' "$out" | tr '\n' ' ')"`)
	if !strings.Contains(r.stdout, "rc=7 out=[out err]") {
		t.Errorf("a command that ends in time: %s", r.all())
	}

	// A grandchild that outlives the command does not keep the substitution open.
	start = time.Now()
	r = sh(t, nil, `out=$(smoke_timeout 5 bash -c 'sleep 20 & echo hi'); printf '[%s]\n' "$out"`)
	if !strings.Contains(r.stdout, "[hi]") || time.Since(start) > 10*time.Second {
		t.Errorf("a grandchild held the substitution (%v): %s", time.Since(start), r.all())
	}
}

// Without a way to isolate the process group the diagnostic is not run (fail
// closed): there is no unsafe fallback.
func TestSmokeTimeoutDeclinesWithoutPerl(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	// A PATH that has bash and sleep (the helper needs them) but no perl.
	bin := filepath.Join(dir, "bin")
	for _, tool := range []string{"bash", "sleep", "mktemp", "cat", "rm", "touch", "kill"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.Symlink(p, filepath.Join(bin, tool))
		}
	}
	r := sh(t, []string{"PATH=" + bin, "MARKER=" + marker}, `smoke_timeout 2 bash -c ': >"$MARKER"' || echo "rc=$?"`)
	if fileExists(marker) {
		t.Errorf("the diagnostic ran without process group isolation: %s", r.all())
	}
	if !strings.Contains(r.stdout, "rc=125") {
		t.Errorf("a declined diagnostic gives 125 and says why: %s", r.all())
	}
}

// Escaped quotes inside a quoted value do not end it: the whole value is
// hidden, in log style (key=value) and in JSON, with several backslashes, with
// unicode escapes, nested in an escaped JSON string, and with the quote left
// open (cut off or continued on the next line): then the rest of the line and
// every later line is hidden.
func TestSmokeRedactEscapedQuotes(t *testing.T) {
	const sec = "hunter2-top-secret-value"
	cases := []struct{ name, in string }{
		{"log escaped quote", `level=ERROR password="one\"` + sec + `" user=anna`},
		{"log pairs then an escaped quote", `password="a\\\\\"` + sec + `" user=anna`},
		{"log three backslashes", `password="a\\\"` + sec + `"`},
		{"json escaped quote", `{"password":"one\"` + sec + `","user":"anna"}`},
		{"json spaces", `{ "token" : "one\"two\"` + sec + `" }`},
		{"json unicode", `{"secret":"\u0022` + sec + `\u0022 \"` + sec + `"}`},
		{"json value with a backslash at the end", `{"api_key":"C:\\dir\\` + sec + `"}`},
		{"single quotes", `token='it\'s ` + sec + `'`},
		{"nested json string", `msg="{\"password\":\"` + sec + `\"}" level=ERROR`},
		{"nested json double escaped", `msg="{\\\"password\\\":\\\"` + sec + `\\\"}"`},
		{"unterminated", `password="one\"` + sec},
		{"unterminated trailing backslash", `password="` + sec + `\`},
		{"bearer quoted", `Authorization: Bearer "one\"` + sec + `"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := sh(t, []string{"IN=" + c.in}, `printf '%s\n' "$IN" | smoke_redact`)
			if r.code != 0 {
				t.Fatalf("smoke_redact: %s", r.all())
			}
			for _, leak := range []string{sec, "hunter2", "top-secret", "secret-value"} {
				if strings.Contains(r.stdout, leak) {
					t.Errorf("leaks %q:\n in:  %s\n out: %s", leak, c.in, r.stdout)
				}
			}
			if !strings.Contains(r.stdout, "[redacted]") {
				t.Errorf("nothing redacted: %s", r.stdout)
			}
		})
	}

	// What is not a secret stays readable, also after a closed secret.
	r := sh(t, []string{"IN=" + `password="a\\" user=anna code=3`}, `printf '%s\n' "$IN" | smoke_redact`)
	if !strings.Contains(r.stdout, "user=anna code=3") {
		t.Errorf("text after a closed value must stay: %s", r.stdout)
	}

	// A value that continues on the next lines: the lines after an unterminated
	// quote are hidden (conservative), a line before it is not.
	in := "before=1\npassword=\"line one\n" + sec + "\nuser=anna\n"
	r = sh(t, []string{"IN=" + in}, `printf '%s' "$IN" | smoke_redact`)
	if strings.Contains(r.stdout, "hunter2") || strings.Contains(r.stdout, "user=anna") || !strings.Contains(r.stdout, "before=1") {
		t.Errorf("multi-line value: %s", r.stdout)
	}
}

// An unquoted value that contains a quote ends at no quote: the suffix after it
// is not a separate word. Everything from the quote to the end of the line is
// hidden (conservative), also when the quote is escaped or the unquoted value
// sits inside a quoted text; what stands before the key stays readable.
func TestSmokeRedactUnquotedValueWithAQuote(t *testing.T) {
	cases := []struct{ name, in, keep string }{
		{"escaped quote", `password=abc\"hunter2-x`, "password="},
		{"escaped quote then more words", `user=anna password=abc\"hunter2-x tail-secret`, "user=anna"},
		{"inside a quoted command", `cmd="curl -d password=abc\"hunter2-x"`, `cmd="curl -d password=`},
		{"apostrophe", `password=it'shunter2`, "password="},
		{"apostrophe and spaces", `token=it's hunter2 more`, "token="},
		{"json style unquoted", `{"api_key":abc"hunter2-x}`, `{"api_key":`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := sh(t, []string{"IN=" + c.in}, `printf '%s\n' "$IN" | smoke_redact`)
			if r.code != 0 {
				t.Fatalf("smoke_redact: %s", r.all())
			}
			for _, leak := range []string{"hunter2", "tail-secret", "shunter"} {
				if strings.Contains(r.stdout, leak) {
					t.Errorf("leaks %q:\n in:  %s\n out: %s", leak, c.in, r.stdout)
				}
			}
			if !strings.Contains(r.stdout, "[redacted]") || !strings.Contains(r.stdout, c.keep) {
				t.Errorf("want [redacted] and %q:\n in:  %s\n out: %s", c.keep, c.in, r.stdout)
			}
		})
	}
	// A value without a quote still stops at its separator.
	r := sh(t, []string{"IN=" + `password=abc,user=anna code=3`}, `printf '%s\n' "$IN" | smoke_redact`)
	if !strings.Contains(r.stdout, "user=anna code=3") || strings.Contains(r.stdout, "abc") {
		t.Errorf("a plain value and what follows it: %s", r.stdout)
	}
}

// Every form of an Authorization credential is hidden, not only Bearer: Basic
// (a short base64 text the long-run rule does not see), Digest with its
// parameters, and any other scheme. Text before the header stays.
func TestSmokeRedactAuthorizationCredentials(t *testing.T) {
	cases := []struct{ name, in, secret string }{
		{"basic", `Authorization: Basic dXNlcjpwdw==`, "dXNlcjpwdw"},
		{"basic lower case", `authorization: basic dXNlcjpwdw==`, "dXNlcjpwdw"},
		{"proxy", `Proxy-Authorization: Basic dXNlcjpwdw==`, "dXNlcjpwdw"},
		{"digest", `Authorization: Digest username="anna", realm="r", response="deadbeef01", nonce=abc123`, "deadbeef01"},
		{"token scheme", `Authorization: Token tok3n-s3cr3t`, "s3cr3t"},
		{"negotiate", `authorization=Negotiate YIIabc`, "YIIabc"},
		{"json header", `{"Authorization":"Basic dXNlcjpwdw=="}`, "dXNlcjpwdw"},
		{"bearer", `Authorization: Bearer ` + fakeShort, fakeShort},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := sh(t, []string{"IN=" + "GET /x user=anna " + c.in}, `printf '%s\n' "$IN" | smoke_redact`)
			if r.code != 0 {
				t.Fatalf("smoke_redact: %s", r.all())
			}
			if strings.Contains(r.stdout, c.secret) {
				t.Errorf("leaks %q: %s", c.secret, r.stdout)
			}
			if !strings.Contains(r.stdout, "GET /x user=anna") || !strings.Contains(r.stdout, "[redacted]") {
				t.Errorf("the text before the header must stay: %s", r.stdout)
			}
		})
	}
}

// The marker that stands for an unterminated quoted secret is a character that
// bash word expansion treats as ordinary (STX, not SOH, the CTLESC byte): it is
// removed from the input first, so a log that contains control characters
// cannot forge it, and it never reaches the output.
func TestSmokeRedactMarkerIsNotForgeable(t *testing.T) {
	for _, ctl := range []string{"\x01", "\x02"} {
		in := "a" + ctl + "b password=\"" + "hunter2-x\nnext line\n"
		r := sh(t, []string{"IN=" + in}, `printf '%s' "$IN" | smoke_redact`)
		if strings.Contains(r.stdout, "hunter2") || strings.Contains(r.stdout, "next line") {
			t.Errorf("control %q: %s", ctl, r.stdout)
		}
		if !strings.Contains(r.stdout, "the lines after an unterminated quoted secret") {
			t.Errorf("control %q: the notice is missing: %q", ctl, r.stdout)
		}
		if strings.Contains(r.stdout, "\x02") {
			t.Errorf("the marker character is left in the output (control %q): %q", ctl, r.stdout)
		}
	}
}
