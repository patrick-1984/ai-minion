package winprobe

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const scScript = `#!/usr/bin/env bash
# fake sc.exe: "queryex NAME" answers the next line of sc.seq ("STATE PID START";
# the last line repeats) in the format of sc.exe, CRLF included, followed by a
# lot of text like a real query of a service with many details would not have,
# so that a reader that stops early breaks the writer.
d=$FAKE_DIR
case "$1" in
queryex)
  n=$(cat "$d/sc.n" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "$d/sc.n"
  line=$(sed -n "${n}p" "$d/sc.seq"); [ -n "$line" ] || line=$(tail -n 1 "$d/sc.seq")
  # shellcheck disable=SC2086
  set -- $line
  echo "$3" > "$d/cur_start"
  names=("" STOPPED START_PENDING STOP_PENDING RUNNING)
  printf 'SERVICE_NAME: MinionWorker \r\n        TYPE               : 10  WIN32_OWN_PROCESS  \r\n'
  printf '        STATE              : %s  %s \r\n' "$1" "${names[$1]:-UNKNOWN}"
  printf '        WIN32_EXIT_CODE    : 0  (0x0)\r\n        PID                : %s\r\n' "$2"
  if [ -f "$d/sc.pad" ]; then
    i=0; while [ "$i" -lt 4000 ]; do printf '        FLAGS              : padding line %s of the output of sc.exe\r\n' "$i"; i=$((i + 1)); done
  fi
  [ -f "$d/sc.rc" ] && { echo "sc failed" >&2; exit "$(cat "$d/sc.rc")"; }
  exit 0
  ;;
esac
exit 9
`

const startScript = `#!/usr/bin/env bash
# fake process creation time: the START of the line fake sc answered last.
cat "$FAKE_DIR/cur_start"
`

const taskkillScript = `#!/usr/bin/env bash
echo "$@" >> "$FAKE_DIR/taskkill.args"
if [ -f "$FAKE_DIR/taskkill.rc" ]; then echo "ERROR: Access is denied." >&2; exit "$(cat "$FAKE_DIR/taskkill.rc")"; fi
echo "SUCCESS: The process with PID $3 has been terminated."
`

const minionScript = `#!/usr/bin/env bash
[ "$1 $2 $3" = "worker status --json" ] || exit 9
echo "$*" >> "$FAKE_DIR/minion.args"
cat "$FAKE_DIR/status.json"
[ -f "$FAKE_DIR/status.err" ] && cat "$FAKE_DIR/status.err" >&2
[ -f "$FAKE_DIR/status.rc" ] && exit "$(cat "$FAKE_DIR/status.rc")"
exit 0
`

const diagScript = `#!/usr/bin/env bash
echo "DIAG-MARKER: sc qfailure, event log"
`

const liveJSON = "{\n  \"connection\": {\n    \"state\": \"idle\",\n    \"live\": true\n  },\n  \"platform\": \"windows\"\n}\n"

type fixture struct {
	t    *testing.T
	dir  string
	env  []string
	seq  []string
	json string
}

func newFixture(t *testing.T, seq ...string) *fixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir, seq: seq, json: liveJSON}
	for name, body := range map[string]string{"sc": scScript, "start": startScript, "taskkill": taskkillScript, "minion": minionScript, "diag": diagScript} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.env = []string{
		"FAKE_DIR=" + dir,
		"PROBE_SC=" + filepath.Join(dir, "sc"),
		"PROBE_START_TIME=" + filepath.Join(dir, "start"),
		"PROBE_TASKKILL=" + filepath.Join(dir, "taskkill"),
		"PROBE_MINION=" + filepath.Join(dir, "minion"),
		"PROBE_DIAG=" + filepath.Join(dir, "diag"),
		"PROBE_DIR=" + filepath.Join(dir, "out"),
		"PROBE_INTERVAL=0",
		"PROBE_ATTEMPTS=4",
	}
	return f
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(name string) string {
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

// run executes the probe and returns its exit code and combined output.
func (f *fixture) run(extraEnv ...string) (int, string) {
	f.t.Helper()
	f.write("sc.seq", strings.Join(f.seq, "\n")+"\n")
	f.write("status.json", f.json)
	script, err := filepath.Abs(filepath.Join("..", "windows-service-restart-probe.sh"))
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Env = append(append(os.Environ(), f.env...), extraEnv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		f.t.Fatalf("run the probe: %v", err)
	}
	return code, out.String()
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// The producers write a lot after the text the old probe looked for
// (grep -q stops at the first match; under pipefail the writer that fails on a
// closed pipe made the pipeline false). A killed instance, then a new one.
func TestNewInstanceIsAcceptedWithLargeSCAndStatusOutput(t *testing.T) {
	f := newFixture(t,
		"4 9180 T1", // before the kill
		"1 0 -",     // poll 1: stopped
		"2 0 -",     // poll 2: start pending
		"4 5000 T2", // poll 3: the new instance
		"4 5000 T2", // the check after the status
	)
	f.write("sc.pad", "x")
	f.json = strings.Replace(liveJSON, "\"platform\"", "\"padding\": \""+strings.Repeat("p", 300000)+"\",\n  \"platform\"", 1)
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	mustContain(t, out, "process 9180 (created T1) -> process 5000 (created T2)")
	if got := strings.TrimSpace(f.read("taskkill.args")); got != "/F /PID 9180" {
		t.Errorf("taskkill got %q, want /F /PID 9180", got)
	}
	if got := f.read("minion.args"); got != "worker status --json\n" {
		t.Errorf("the status was asked %q, want exactly once", got)
	}
}

// Windows reuses process ids: the same id with another creation time is a new
// process; the same id with the same creation time is still the killed one.
func TestReusedProcessIDWithANewCreationTimeIsANewInstance(t *testing.T) {
	f := newFixture(t,
		"4 777 T1", // before the kill
		"4 777 T1", // poll 1: still the killed process
		"4 777 T2", // poll 2: the id is reused by the new instance
	)
	code, out := f.run()
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	mustContain(t, out, "process 777 (created T1) -> process 777 (created T2)")
	if got := strings.Count(f.read("minion.args"), "worker status"); got != 1 {
		t.Errorf("the status was asked %d times, want 1 (not for the killed process)", got)
	}
}

// RUNNING alone is not a restart: the killed process may still be the one the
// SCM reports.
func TestRunningKilledInstanceIsNotARestartAndTheFailureIsDiagnosed(t *testing.T) {
	f := newFixture(t, "4 9180 T1")
	code, out := f.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	mustContain(t, out,
		"the service was not restarted after its process was killed",
		"still the killed instance, process 9180 created T1",
		"STATE              : 4  RUNNING", // the last queryex output
		"PID                : 9180",
		"DIAG-MARKER")
	if strings.Contains(f.read("minion.args"), "worker") {
		t.Errorf("the CLI was asked about the status of the killed instance")
	}
}

// A new instance whose status cannot be read keeps the exit status and the error
// text of the last attempt for the diagnosis.
func TestNewInstanceThatDoesNotAnswerKeepsTheLastStatusError(t *testing.T) {
	f := newFixture(t, "4 9180 T1", "4 5000 T2")
	f.write("status.rc", "1")
	f.write("status.err", "cannot reach the Worker: pipe not found\n")
	code, out := f.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	mustContain(t, out,
		"new instance, process 5000 created T2, status exit 1, live no",
		"--- status.err", "cannot reach the Worker: pipe not found",
		"--- status.rc", "DIAG-MARKER")
}

func TestStatusWithProblemsIsNotHealthy(t *testing.T) {
	f := newFixture(t, "4 9180 T1", "4 5000 T2")
	f.json = strings.Replace(liveJSON, "\"platform\"", "\"problems\": [\"cannot read the enrollment\"],\n  \"platform\"", 1)
	code, out := f.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	mustContain(t, out, "live no", "cannot read the enrollment")
}

// The status has no process id: it is accepted only if the SCM reports the same
// instance after the CLI asked.
func TestStatusIsRejectedWhenTheInstanceChangesWhileItIsRead(t *testing.T) {
	f := newFixture(t,
		"4 9180 T1", // before the kill
		"4 5000 T2", // poll 1: a new instance ...
		"4 5001 T3", // ... which the SCM has replaced when the status is done
	)
	code, out := f.run("PROBE_ATTEMPTS=1")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	mustContain(t, out, "the instance changed while the status was read (now process 5001 state 4)")
	// With more polls the second instance, which stays, is accepted.
	f2 := newFixture(t, "4 9180 T1", "4 5000 T2", "4 5001 T3")
	code, out = f2.run("PROBE_ATTEMPTS=3")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	mustContain(t, out, "process 9180 (created T1) -> process 5001 (created T3)")
}

func TestKillThatFailsStopsTheProbeBeforeAnyWaiting(t *testing.T) {
	f := newFixture(t, "4 9180 T1")
	f.write("taskkill.rc", "1")
	code, out := f.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	mustContain(t, out, "taskkill of process 9180 exited with 1", "ERROR: Access is denied.")
	if got := strings.TrimSpace(f.read("sc.n")); got != "1" {
		t.Errorf("the SCM was queried %s times, want 1 (no waiting after a failed kill)", got)
	}
}

func TestNoInstanceBeforeTheKillIsAFailureWithTheSCMError(t *testing.T) {
	f := newFixture(t, "1 0 -")
	f.write("sc.rc", "1060")
	code, out := f.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	mustContain(t, out, "cannot read a running instance of MinionWorker before the kill", "sc failed")
	if f.read("taskkill.args") != "" {
		t.Errorf("a process was killed although no instance was read")
	}
}

// The restart step must run the probe: the script is not dead code, and the
// workflow keeps no probe that pipes a producer into grep -q.
func TestWorkflowRunsTheProbeAndPipesNoServiceQueryIntoGrep(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "windows.yml"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	if !strings.Contains(doc, "run: scripts/windows-service-restart-probe.sh") {
		t.Error("windows.yml does not run scripts/windows-service-restart-probe.sh")
	}
	for i, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "sc.exe") && strings.Contains(line, "| grep") {
			t.Errorf("windows.yml:%d pipes sc.exe into grep: %s", i+1, strings.TrimSpace(line))
		}
	}
	st, err := os.Stat(filepath.Join("..", "windows-service-restart-probe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Error("the probe script is not executable (the workflow runs it directly)")
	}
}
