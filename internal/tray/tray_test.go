package tray

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake `minion`: with FAKE_MINION_LOG set it
// records its argv and stdin as one JSON line, prints the canned reply for the
// command (FAKE_MINION_DIR/<args joined by _>.out) and fails when a
// <...>.fail file exists.
func TestMain(m *testing.M) {
	if log := os.Getenv("FAKE_MINION_LOG"); log != "" {
		os.Exit(fakeMinion(log, os.Getenv("FAKE_MINION_DIR")))
	}
	os.Exit(m.Run())
}

func fakeMinion(log, dir string) int {
	stdin := ""
	if st, _ := os.Stdin.Stat(); st != nil && st.Mode()&os.ModeCharDevice == 0 {
		stdin, _ = bufio.NewReader(os.Stdin).ReadString(0)
	}
	rec, _ := json.Marshal(call{Args: os.Args[1:], Stdin: stdin})
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	_, _ = f.Write(append(rec, '\n'))
	_ = f.Close()
	key := filepath.Join(dir, strings.Join(os.Args[1:], "_"))
	if _, err := os.Stat(key + ".sleep"); err == nil {
		time.Sleep(20 * time.Second)
	}
	if msg, err := os.ReadFile(key + ".fail"); err == nil {
		_, _ = os.Stderr.Write(msg)
		return 1
	}
	if out, err := os.ReadFile(key + ".out"); err == nil {
		_, _ = os.Stdout.Write(out)
	}
	return 0
}

type call struct {
	Args  []string
	Stdin string
}

type fakeDialogs struct {
	token     string
	tokenOK   bool
	confirm   bool
	confirms  int
	pickWant  []string
	pickOK    bool
	pickAll   []string
	pickCheck []string
	errs      []string
	infos     []string
}

func (d *fakeDialogs) Token() (string, bool) { return d.token, d.tokenOK }
func (d *fakeDialogs) Confirm(string) bool   { d.confirms++; return d.confirm }
func (d *fakeDialogs) Pick(_ string, all, checked []string) ([]string, bool) {
	d.pickAll, d.pickCheck = all, checked
	return d.pickWant, d.pickOK
}
func (d *fakeDialogs) Info(s string)  { d.infos = append(d.infos, s) }
func (d *fakeDialogs) Error(s string) { d.errs = append(d.errs, s) }

type fakeView struct {
	s    Status
	err  error
	seen int
}

func (v *fakeView) Show(s Status, err error) { v.s, v.err, v.seen = s, err, v.seen+1 }

type rig struct {
	c   *Controller
	d   *fakeDialogs
	v   *fakeView
	dir string
	log string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.jsonl")
	t.Setenv("FAKE_MINION_LOG", log)
	t.Setenv("FAKE_MINION_DIR", dir)
	r := &rig{d: &fakeDialogs{}, v: &fakeView{}, dir: dir, log: log}
	r.c = &Controller{R: ExecRunner{Path: self}, D: r.d, V: r.v}
	return r
}

func (r *rig) reply(t *testing.T, args, out string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, strings.ReplaceAll(args, " ", "_")+".out"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) fail(t *testing.T, args, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, strings.ReplaceAll(args, " ", "_")+".fail"), []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) calls(t *testing.T) []call {
	t.Helper()
	b, _ := os.ReadFile(r.log)
	var out []call
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var c call
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (r *rig) argv(t *testing.T) []string {
	var out []string
	for _, c := range r.calls(t) {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

const statusJSON = `{"hub_url":"https://hub.example","paired":true,"paused":false,"new_sessions":"on","sessions":2,
"hub_fingerprint_words":"a b c d e f","hub_check":"fingerprint","connection":{"state":"connected","live":true}}`

func TestRefreshShowsStatus(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker status --json", statusJSON)
	r.c.Refresh()
	s := r.v.s
	if s.StatusLine() != "Worker running, connected" || s.HubLine() != "Hub: https://hub.example" || s.KeysLine() != "Hub key: a b c d e f" || s.Mismatch() {
		t.Fatalf("%q %q %q", s.StatusLine(), s.HubLine(), s.KeysLine())
	}
	if got := r.argv(t); !reflect.DeepEqual(got, []string{"worker status --json"}) {
		t.Fatal(got)
	}
}

func TestMismatchIsProminent(t *testing.T) {
	for _, js := range []string{
		`{"paired":true,"hub_check":"mismatch","hub_fingerprint_words":"a b c d e f","connection":{"live":false}}`,
		`{"paired":true,"alarm":"hub_identity_changed","hub_fingerprint_words":"a b c d e f","connection":{"live":true}}`,
	} {
		r := newRig(t)
		r.reply(t, "worker status --json", js)
		r.c.Refresh()
		if !r.v.s.Mismatch() || !strings.Contains(r.v.s.KeysLine(), "MISMATCH") {
			t.Fatalf("%s: %q", js, r.v.s.KeysLine())
		}
	}
}

func TestRefreshFailureReachesView(t *testing.T) {
	r := newRig(t)
	r.fail(t, "worker status --json", "boom")
	r.c.Refresh()
	if r.v.err == nil || !strings.Contains(r.v.err.Error(), "boom") {
		t.Fatal(r.v.err)
	}
}

func TestStartRunsCommandAndRefreshes(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker status --json", statusJSON)
	r.c.Start()
	if got := r.argv(t); !reflect.DeepEqual(got, []string{"worker start", "worker status --json"}) {
		t.Fatal(got)
	}
}

func TestStartFailureShowsDialogAndStillRefreshes(t *testing.T) {
	r := newRig(t)
	r.fail(t, "worker start", "nothing installed: run minion worker up")
	r.c.Start()
	if len(r.d.errs) != 1 || !strings.Contains(r.d.errs[0], "nothing installed") {
		t.Fatal(r.d.errs)
	}
	if got := r.argv(t); len(got) != 2 || got[1] != "worker status --json" {
		t.Fatal(got)
	}
}

func TestStopConfirmsWhenSessionsCouldEnd(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker status --json", statusJSON) // 2 sessions
	r.c.Stop()                                     // not confirmed
	if r.d.confirms != 1 {
		t.Fatal("no confirmation")
	}
	for _, a := range r.argv(t) {
		if a == "worker down" {
			t.Fatal("stopped without confirmation")
		}
	}
	r.d.confirm = true
	r.c.Stop()
	found := false
	for _, a := range r.argv(t) {
		found = found || a == "worker down"
	}
	if !found {
		t.Fatal(r.argv(t))
	}
}

func TestStopWithoutSessionsDoesNotAsk(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker status --json", `{"sessions":0,"session_host":"tmux","connection":{"live":true}}`)
	r.c.Stop()
	if r.d.confirms != 0 {
		t.Fatal("asked")
	}
	if got := r.argv(t); got[1] != "worker down" {
		t.Fatal(got)
	}
}

func TestConPTYAlwaysConfirms(t *testing.T) {
	s := Status{SessionHost: "conpty"}
	s.Connection.Live = true
	zero := 0
	s.Sessions = &zero
	if !s.SessionsCouldEnd() {
		t.Fatal("conpty sessions live in the Worker")
	}
}

func TestPauseResume(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker status --json", statusJSON)
	r.c.TogglePause()
	r.reply(t, "worker status --json", strings.Replace(statusJSON, `"paused":false`, `"paused":true`, 1))
	r.c.TogglePause()
	var got []string
	for _, a := range r.argv(t) {
		if a != "worker status --json" {
			got = append(got, a)
		}
	}
	if !reflect.DeepEqual(got, []string{"worker pause", "worker resume"}) {
		t.Fatal(got)
	}
}

func TestSetupSendsTokenOnStdinOnly(t *testing.T) {
	r := newRig(t)
	r.d.token, r.d.tokenOK = "  SECRET-TOKEN \n", true
	r.reply(t, "worker status --json", statusJSON)
	r.c.Setup()
	cs := r.calls(t)
	if !reflect.DeepEqual(cs[0].Args, []string{"worker", "up", "--join", "-"}) || cs[0].Stdin != "SECRET-TOKEN\n" {
		t.Fatalf("%+v", cs[0])
	}
	for _, c := range cs {
		if strings.Contains(strings.Join(c.Args, " "), "SECRET") {
			t.Fatal("token in argv")
		}
	}
	if len(r.d.infos) != 1 {
		t.Fatal(r.d.infos)
	}
}

func TestSetupCancelledOrEmptyRunsNothing(t *testing.T) {
	r := newRig(t)
	r.d.token, r.d.tokenOK = "x", false
	r.c.Setup()
	r.d.token, r.d.tokenOK = "  ", true
	r.c.Setup()
	if len(r.calls(t)) != 0 {
		t.Fatal(r.argv(t))
	}
}

func TestSetupFailureShowsMessage(t *testing.T) {
	r := newRig(t)
	r.d.token, r.d.tokenOK = "tok", true
	r.fail(t, "worker up --join -", "the invitation has expired")
	r.c.Setup()
	if len(r.d.errs) != 1 || !strings.Contains(r.d.errs[0], "expired") || len(r.d.infos) != 0 {
		t.Fatal(r.d.errs, r.d.infos)
	}
}

const shareJSON = `{"mode":"select","revision":"7","locked":false,"selected":["a"],"sessions_known":true,
"sessions":[{"name":"a","shared":true},{"name":"-rm","shared":false},{"name":"b c","shared":false}]}`

func TestSharePickerPutsNamesAfterDashDash(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker config share list --json", shareJSON)
	r.reply(t, "worker status --json", statusJSON)
	r.d.pickOK, r.d.pickWant = true, []string{"a", "-rm", "b c"}
	r.c.ShareSessions()
	if !reflect.DeepEqual(r.d.pickAll, []string{"a", "-rm", "b c"}) || !reflect.DeepEqual(r.d.pickCheck, []string{"a"}) {
		t.Fatal(r.d.pickAll, r.d.pickCheck)
	}
	cs := r.calls(t)
	want := []string{"worker", "config", "share", "select", "--revision", "7", "--", "a", "-rm", "b c"}
	if !reflect.DeepEqual(cs[1].Args, want) {
		t.Fatalf("%q", cs[1].Args)
	}
}

func TestShareNothingChosenIsNone(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker config share list --json", shareJSON)
	r.d.pickOK = true
	r.c.ShareSessions()
	want := []string{"worker", "config", "share", "none", "--revision", "7"}
	if cs := r.calls(t); !reflect.DeepEqual(cs[1].Args, want) {
		t.Fatalf("%q", cs[1].Args)
	}
}

func TestShareCancelledChangesNothing(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker config share list --json", shareJSON)
	r.c.ShareSessions()
	if got := r.argv(t); len(got) != 1 {
		t.Fatal(got)
	}
}

func TestShareStaleRevisionExplainsAndKeepsChoice(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker config share list --json", shareJSON)
	r.fail(t, "worker config share select --revision 7 -- a", "share choice changed (revision 8)")
	r.d.pickOK, r.d.pickWant = true, []string{"a"}
	// The second listing, after the failure, shows the newer revision.
	r.c.R = &afterFirst{inner: r.c.R, key: "worker config share list --json", second: strings.Replace(shareJSON, `"7"`, `"8"`, 1)}
	r.c.ShareSessions()
	if len(r.d.errs) != 1 || !strings.Contains(r.d.errs[0], "changed while you were choosing") {
		t.Fatal(r.d.errs)
	}
}

// afterFirst answers the second call of key with a canned output.
type afterFirst struct {
	inner  Runner
	key    string
	second string
	n      int
}

func (a *afterFirst) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	if strings.Join(args, " ") == a.key {
		a.n++
		if a.n == 2 {
			return a.second, nil
		}
	}
	return a.inner.Run(ctx, stdin, args...)
}

func TestShareOtherFailureShowsItsMessage(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker config share list --json", shareJSON)
	r.fail(t, "worker config share none --revision 7", "state is locked")
	r.d.pickOK = true
	r.c.ShareSessions()
	if len(r.d.errs) != 1 || !strings.Contains(r.d.errs[0], "state is locked") {
		t.Fatal(r.d.errs)
	}
}

func TestShareListFailure(t *testing.T) {
	r := newRig(t)
	r.fail(t, "worker config share list --json", "no state")
	r.c.ShareSessions()
	if len(r.d.errs) != 1 || r.d.confirms != 0 {
		t.Fatal(r.d.errs)
	}
}

func TestShareWithoutKnownSessionsOffersSavedSelection(t *testing.T) {
	l := ShareList{Selected: []string{"x"}}
	all, checked := l.Names()
	if !reflect.DeepEqual(all, []string{"x"}) || !reflect.DeepEqual(checked, []string{"x"}) {
		t.Fatal(all, checked)
	}
}

func TestNewSessionsToggle(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker status --json", statusJSON) // on
	r.c.ToggleNewSessions()
	r.reply(t, "worker status --json", strings.Replace(statusJSON, `"new_sessions":"on"`, `"new_sessions":"off"`, 1))
	r.c.ToggleNewSessions()
	var got []string
	for _, a := range r.argv(t) {
		if strings.Contains(a, "config set") {
			got = append(got, a)
		}
	}
	if !reflect.DeepEqual(got, []string{"worker config set allow_new_sessions off", "worker config set allow_new_sessions on"}) {
		t.Fatal(got)
	}
}

func TestNewSessionsFailureShown(t *testing.T) {
	r := newRig(t)
	r.reply(t, "worker status --json", statusJSON)
	r.fail(t, "worker config set allow_new_sessions off", "cannot write the config")
	r.c.ToggleNewSessions()
	if len(r.d.errs) != 1 {
		t.Fatal(r.d.errs)
	}
}

func TestRunnerTimeout(t *testing.T) {
	self, _ := os.Executable()
	dir := t.TempDir()
	t.Setenv("FAKE_MINION_LOG", filepath.Join(dir, "l"))
	t.Setenv("FAKE_MINION_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "slow.sleep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := ExecRunner{Path: self, Timeout: 300 * time.Millisecond}.Run(context.Background(), "", "slow")
	var ce *CmdError
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "did not finish") || time.Since(start) > 10*time.Second {
		t.Fatal(err, time.Since(start))
	}
	if _, err := (ExecRunner{Path: filepath.Join(dir, "nope")}).Run(context.Background(), "", "x"); !errors.As(err, &ce) {
		t.Fatal(err)
	}
}
