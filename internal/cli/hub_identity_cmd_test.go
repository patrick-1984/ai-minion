//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/hub/adminsock"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
)

type idCall struct {
	method string
	actor  hubid.Actor
	pass   string
	new    string
	opts   hubid.IssueOptions
}

// idService is the identity service behind a real admin socket.
type idService struct {
	mu    sync.Mutex
	calls []idCall
	err   error
}

func (f *idService) rec(c idCall) (hubid.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	return hubid.Result{Kind: c.method, Serial: 42, RootFingerprint: "ROOTFP", OpFingerprint: "OPFP",
		NotBefore: time.Unix(1_700_000_000, 0), NotAfter: time.Unix(1_720_000_000, 0)}, f.err
}

func (f *idService) SetPassphrase(_ context.Context, a hubid.Actor, p, _ string, o hubid.IssueOptions) (hubid.Result, error) {
	return f.rec(idCall{"passphrase_set", a, p, "", o})
}

func (f *idService) Reissue(_ context.Context, a hubid.Actor, p string, o hubid.IssueOptions) (hubid.Result, error) {
	return f.rec(idCall{"reissue", a, p, "", o})
}

func (f *idService) RotateMaster(_ context.Context, a hubid.Actor, p, n, _ string, o hubid.IssueOptions) (hubid.Result, error) {
	return f.rec(idCall{"rotate", a, p, n, o})
}

func (f *idService) ChangePassphrase(_ context.Context, a hubid.Actor, p, n, _ string) (hubid.Result, error) {
	return f.rec(idCall{"passphrase_change", a, p, n, hubid.IssueOptions{}})
}

func (f *idService) Status(context.Context) (hubid.Status, error) {
	return hubid.Status{State: hubid.StateEstablished, Serving: true, Serial: 42, RootFingerprint: "ROOTFP",
		NotAfter: time.Unix(1_720_000_000, 0), NotBefore: time.Unix(1_700_000_000, 0), DaysLeft: 12, Warning: hubid.WarnWarning,
		Web: hubid.ScopeStatus{Locked: true, LockedUntil: time.Now().Add(time.Hour)}}, f.err
}

func (f *idService) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func (f *idService) last(t *testing.T) idCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the Hub was not called")
	}
	return f.calls[len(f.calls)-1]
}

type idEnv struct {
	dir    string
	svc    *idService
	dials  int
	secret []string // answers of readSecret, in order
	line   string
	asked  []string
	tty    bool
}

func newIDEnv(t *testing.T, withHub bool) *idEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "cl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := &idEnv{dir: dir, svc: &idService{}, tty: true}
	if withHub {
		ln, err := adminsock.Listen(adminsock.Path(dir))
		if err != nil {
			t.Fatal(err)
		}
		srv := adminsock.NewServer(e.svc, nil)
		done := make(chan struct{})
		go func() { defer close(done); _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close(); <-done })
	}
	return e
}

func (e *idEnv) deps() identityDeps {
	d := defaultIdentityDeps()
	d.lookupEnv = envOf(map[string]string{"MINION_DATA_DIR": e.dir})
	d.isTerminal = func() bool { return e.tty }
	d.readSecret = func(prompt string) (string, error) {
		e.asked = append(e.asked, prompt)
		if len(e.secret) == 0 {
			return "", errors.New("no more answers")
		}
		s := e.secret[0]
		e.secret = e.secret[1:]
		return s, nil
	}
	d.readLine = func(prompt string) (string, error) { e.asked = append(e.asked, prompt); return e.line, nil }
	d.timeout = 20 * time.Second
	inner := d.dial
	d.dial = func(ctx context.Context, p string) (net.Conn, error) { e.dials++; return inner(ctx, p) }
	return d
}

func (e *idEnv) run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	identity := &cobra.Command{Use: "identity", SilenceUsage: true, SilenceErrors: true}
	addIdentityCommands(identity, e.deps())
	root := &cobra.Command{Use: "minion", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(identity)
	var out, errb bytes.Buffer
	code = execute(context.Background(), root, append([]string{"identity"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestIdentityCommandsRefuseWhenStandardInputIsNotATerminal(t *testing.T) {
	for _, args := range [][]string{{"set-passphrase"}, {"reissue"}, {"rotate-master"}, {"rotate"}, {"change-passphrase"}} {
		e := newIDEnv(t, true)
		e.tty = false
		e.secret = []string{"never-read-aaaa"}
		code, out, errText := e.run(t, args...)
		if code != ExitUsage || !strings.Contains(errText, "not a terminal") || out != "" {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, out, errText)
		}
		if e.dials != 0 || e.svc.count() != 0 || len(e.asked) != 0 {
			t.Errorf("%v: the command asked (%v), connected (%d) or called the Hub (%d) without a terminal", args, e.asked, e.dials, e.svc.count())
		}
	}
	// The default dependencies check the real standard input, which is no terminal under go test.
	if defaultIdentityDeps().isTerminal() {
		t.Skip("standard input is a terminal")
	}
	for _, args := range [][]string{{"hub", "identity", "reissue"}, {"hub", "identity", "change-passphrase"}} {
		var out, errb bytes.Buffer
		t.Setenv("MINION_DATA_DIR", t.TempDir())
		if code := Run(context.Background(), append([]string{"minion"}, args...), &out, &errb); code != ExitUsage || !strings.Contains(errb.String(), "not a terminal") {
			t.Errorf("%v: code %d stderr %q", args, code, errb.String())
		}
	}
	// The flags and environment variables do not carry the passphrase.
	for _, args := range [][]string{{"reissue", "--passphrase", "x"}, {"set-passphrase", "--passphrase-file", "/dev/null"}} {
		e := newIDEnv(t, true)
		if code, _, _ := e.run(t, args...); code != ExitUsage {
			t.Errorf("%v: code %d, want usage", args, code)
		}
	}
}

func TestIdentityCommandsSendThePassphraseToTheHubAndShowOnlyPublicValues(t *testing.T) {
	const p1, p2 = "first-long-passphrase", "second-long-passphrase"
	e := newIDEnv(t, true)

	e.secret = []string{p1}
	code, out, errText := e.run(t, "reissue", "--validity", "30", "--serial-floor", "9", "--allow-far-floor")
	if code != ExitOK {
		t.Fatalf("reissue = %d %q %q", code, out, errText)
	}
	c := e.svc.last(t)
	if c.method != "reissue" || c.pass != p1 || c.opts.ValidityDays != 30 || c.opts.SerialFloor != 9 || !c.opts.AllowFarFloor {
		t.Errorf("call = %+v", c)
	}
	if want := fmt.Sprintf("local:%d", os.Getuid()); c.actor.ID != want || c.actor.Channel != hubid.ChannelCLI {
		t.Errorf("actor = %+v, want %s on the cli channel (from the socket peer)", c.actor, want)
	}
	if !strings.Contains(out, "re-issued") || !strings.Contains(out, "ROOTFP") || !strings.Contains(out, "42") {
		t.Errorf("output = %q", out)
	}

	e.secret = []string{p1, p1}
	if code, out, errText = e.run(t, "set-passphrase"); code != ExitOK || e.svc.last(t).method != "passphrase_set" {
		t.Errorf("set-passphrase = %d %q %q", code, out, errText)
	}
	if got := e.svc.last(t); got.pass != p1 {
		t.Errorf("set-passphrase call = %+v", got)
	}

	e.secret, e.line = []string{p1, p2, p2}, "ROTATE"
	if code, out, errText = e.run(t, "rotate-master"); code != ExitOK || e.svc.last(t).method != "rotate" {
		t.Errorf("rotate-master = %d %q %q", code, out, errText)
	}
	if got := e.svc.last(t); got.pass != p1 || got.new != p2 || !strings.Contains(out, "invitation") {
		t.Errorf("rotate call = %+v out %q", got, out)
	}

	e.secret = []string{p2, p1, p1}
	if code, out, errText = e.run(t, "change-passphrase"); code != ExitOK || e.svc.last(t).method != "passphrase_change" {
		t.Errorf("change-passphrase = %d %q %q", code, out, errText)
	}
	// The prompts name the field, never a value; nothing printed contains a passphrase.
	for _, a := range e.asked {
		if strings.Contains(a, p1) || strings.Contains(a, p2) {
			t.Errorf("prompt %q holds a passphrase", a)
		}
	}
	if strings.Contains(out+errText, p1) || strings.Contains(out+errText, p2) {
		t.Error("a passphrase was printed")
	}
}

func TestRotateMasterNeedsTheWordRotate(t *testing.T) {
	e := newIDEnv(t, true)
	e.secret, e.line = []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "bbbbbbbbbbbb"}, "rotate"
	code, _, errText := e.run(t, "rotate-master")
	if code != ExitUsage || e.svc.count() != 0 || !strings.Contains(errText, "ROTATE") {
		t.Errorf("code %d stderr %q calls %d", code, errText, e.svc.count())
	}
}

func TestIdentityCommandsMapHubRefusalsToExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		code int
		text string
	}{
		{&hubid.Error{Code: hubid.CodePassphraseInvalid}, ExitPassphraseInvalid, "not accepted"},
		{&hubid.Error{Code: hubid.CodeRateLimited, RetryAt: time.Now().Add(time.Hour)}, ExitIdentityLimited, "try again later"},
		{&hubid.Error{Code: hubid.CodeConflict, Msg: "a sealed root exists; use re-issue certificate"}, ExitIdentityConflict, "re-issue"},
		{&hubid.Error{Code: hubid.CodeInvalidArgument, Msg: "the passphrase is too short"}, ExitUsage, "too short"},
		{errors.New("disk exploded at /secret/path"), ExitError, ""},
	}
	for _, c := range cases {
		e := newIDEnv(t, true)
		e.svc.err = c.err
		e.secret = []string{"correct horse battery"}
		code, out, errText := e.run(t, "reissue")
		if code != c.code || out != "" || !strings.Contains(errText, c.text) || strings.Contains(errText, "/secret/path") || strings.Contains(errText, "correct horse") {
			t.Errorf("%v: code %d stdout %q stderr %q", c.err, code, out, errText)
		}
	}
}

func TestIdentityCommandsSayWhenNoHubRuns(t *testing.T) {
	e := newIDEnv(t, false)
	e.secret = []string{"correct horse battery"}
	code, _, errText := e.run(t, "reissue")
	if code != ExitHubUnreachable || !strings.Contains(errText, "no running Hub") {
		t.Errorf("code %d stderr %q", code, errText)
	}
	// A regular file at the socket path is not trusted with a passphrase.
	if err := os.WriteFile(adminsock.Path(e.dir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.secret = []string{"correct horse battery"}
	e.dials = 0
	if code, _, _ = e.run(t, "reissue"); code != ExitHubUnreachable || e.dials != 0 {
		t.Errorf("code %d dials %d", code, e.dials)
	}
	if code, _, _ = e.run(t, "status"); code != ExitHubUnreachable {
		t.Errorf("status without a Hub = %d", code)
	}
}

func TestIdentityStatusNeedsNoTerminalAndShowsTheLockout(t *testing.T) {
	e := newIDEnv(t, true)
	e.tty = false
	code, out, errText := e.run(t, "status")
	if code != ExitOK {
		t.Fatalf("status = %d %q", code, errText)
	}
	for _, want := range []string{"B3 established", "ROOTFP", "12 days left", "Warning", "Lockout (web): locked", "Lockout (commands): none"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = e.run(t, "status", "--json")
	if code != ExitOK || !strings.Contains(out, `"state":"B3"`) || strings.Contains(out, "\n\n") {
		t.Errorf("status --json = %d %q", code, out)
	}
	if e.svc.count() != 0 {
		t.Error("status called an operation")
	}
}

func TestTheIdentityCommandIsPartOfTheHubCommandAndKeepsThePrintForm(t *testing.T) {
	root := NewRoot()
	for _, name := range []string{"status", "set-passphrase", "reissue", "rotate-master", "change-passphrase"} {
		cmd, _, err := root.Find([]string{"hub", "identity", name})
		if err != nil || cmd.Name() != name {
			t.Errorf("hub identity %s: %v", name, err)
		}
	}
	if cmd, _, err := root.Find([]string{"hub", "identity", "rotate"}); err != nil || cmd.Name() != "rotate-master" {
		t.Errorf("alias rotate: %v %v", cmd, err)
	}
	// The socket lives in the data directory.
	d := defaultIdentityDeps()
	d.lookupEnv = envOf(map[string]string{"MINION_DATA_DIR": "/data"})
	if p, err := d.socketPath(); err != nil || p != filepath.Join("/data", adminsock.SocketName) {
		t.Errorf("socket path = %q, %v", p, err)
	}
}
