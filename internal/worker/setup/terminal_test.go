package setup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// syncBuffer is a writer that the dialog and the test can use at once.
type syncBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	changed chan struct{} // closed and replaced on every write
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.b.Write(p)
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
	return n, err
}

// waitFor blocks until the output contains want, or the test fails after a
// deadline. It wakes on every write: no polling, no sleeping.
func (s *syncBuffer) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		s.mu.Lock()
		if strings.Contains(s.b.String(), want) {
			s.mu.Unlock()
			return
		}
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("the output never contained %q:\n%s", want, s.String())
		}
	}
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// typist plays the user: lines are written to the dialog's input one at a
// time, each when the dialog asks for the next line. The input stays open (as
// a terminal does) until the test ends.
type typist struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newTypist(t *testing.T) *typist {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	return &typist{r: r, w: w}
}

// type_ writes the lines in order on another goroutine.
func (u *typist) typeLines(lines ...string) {
	go func() {
		for _, l := range lines {
			if _, err := io.WriteString(u.w, l+"\n"); err != nil {
				return
			}
		}
	}()
}

// press writes one line now (for example Enter during the pairing wait).
func (u *typist) press(t *testing.T, line string) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.WriteString(u.w, line+"\n") }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nobody read the key press")
	}
}

type outcome struct {
	res Result
	err error
	out string
}

// runDialog runs the terminal wizard to its end, with a deadline.
func runDialog(t *testing.T, w *Wizard, in io.Reader, interactive bool) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return runDialogCtx(t, ctx, w, in, interactive)
}

func runDialogCtx(t *testing.T, ctx context.Context, w *Wizard, in io.Reader, interactive bool) outcome {
	t.Helper()
	out := &syncBuffer{}
	term := &Terminal{In: in, Out: out, Interactive: interactive}
	res, err := term.Run(ctx, w)
	return outcome{res: res, err: err, out: out.String()}
}

// startDialog runs the dialog on a goroutine and returns what is needed to
// steer and wait for it.
func startDialog(t *testing.T, ctx context.Context, w *Wizard, in io.Reader) (out *syncBuffer, done <-chan outcome) {
	t.Helper()
	out = &syncBuffer{}
	ch := make(chan outcome, 1)
	go func() {
		res, err := (&Terminal{In: in, Out: out, Interactive: true}).Run(ctx, w)
		ch <- outcome{res: res, err: err, out: out.String()}
	}()
	return out, ch
}

func waitDone(t *testing.T, done <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-done:
		return o
	case <-time.After(20 * time.Second):
		t.Fatal("the dialog did not end")
		return outcome{}
	}
}

func contains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func lacks(t *testing.T, out string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(out, w) {
			t.Errorf("output must not contain %q:\n%s", w, out)
		}
	}
}

// countingReader fails the test when anything reads it.
type countingReader struct{ t *testing.T }

func (c *countingReader) Read([]byte) (int, error) {
	c.t.Error("the wizard read its input, but no question had to be asked")
	return 0, io.EOF
}

// ---- tests ----

// The whole happy path, as a non-technical user would type it: Enter for the
// default share choice, the Hub address without a scheme, and the code.
func TestTerminalHappyPathNarrative(t *testing.T) {
	r := newRig([]string{"build", "docs", "notes"}, Provided{})
	u := newTypist(t)
	u.typeLines("", "hub.example:8443") // default (all), then the address
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, done := startDialog(t, ctx, r.w, u.r)
	o := waitDone(t, done)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out,
		"Step 1 of 3: What may the Hub see?",
		"I see 3 tmux sessions:",
		"Configure later from the Hub",
		"Sharing: share all tmux sessions, also the ones created later.",
		"Step 2 of 3: Hub address",
		"(I added https:// in front: https://hub.example:8443)",
		"DNS", "TCP", "TLS", "Protocol",
		"Step 3 of 3: Pairing",
		"Pairing code: MN-K7Q4-XY12",
		"Your choices are saved.",
		"  Pairing: paired to alice",
	)
	if n := strings.Count(o.out, "paired to alice"); n != 2 { // the pairing line and the summary
		t.Errorf("\"paired to alice\" appears %d times, want 2 (pairing line, summary):\n%s", n, o.out)
	}
	if got := r.per.saved(); len(got) != 1 || got[0].Share.Mode != store.ShareAll || got[0].Server != "https://hub.example:8443" {
		t.Errorf("saved = %+v", got)
	}
	if got := r.chk.called(); len(got) != 1 || got[0] != "https://hub.example:8443" {
		t.Errorf("checked = %v", got)
	}
	if !o.res.Pairing.Paired || o.res.Pairing.Owner != "alice" {
		t.Errorf("result = %+v", o.res)
	}
}

func TestTerminalChooseNowTicksAndSavesTheChosenNames(t *testing.T) {
	r := newRig([]string{"build", "docs", "notes"}, Provided{})
	u := newTypist(t)
	// choose; tick 1 and 3; untick 3 again, tick 2... final: 1 and 2? Keep it plain:
	u.typeLines("2", "1 3", "3", "x", "", "") // tick 1,3; untick 3; "x" is not a number; Enter; then Enter at the Hub step = Solo
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out,
		"[ ] 1  build", "[ ] 2  docs", "[ ] 3  notes", // the first list
		"[x] 1  build",
		`"x" is not a number from the list.`,
		"Sharing: share only 1 session: build.",
		"No Hub: the Worker runs alone (Solo).",
	)
	got := r.per.saved()
	if len(got) != 1 || got[0].Share.Mode != store.ShareSelect || len(got[0].Share.Sessions) != 1 || got[0].Share.Sessions[0] != "build" {
		t.Fatalf("saved = %+v, want select [build]", got)
	}
	if got[0].Server != "" {
		t.Errorf("Solo must save no Hub address: %q", got[0].Server)
	}
	if len(r.pair.calls()) != 0 {
		t.Error("a Solo Worker must not pair")
	}
}

func TestTerminalChooseNowNothingTickedAsksAgainAndBackReturns(t *testing.T) {
	r := newRig([]string{"build"}, Provided{})
	u := newTypist(t)
	u.typeLines("2", "", "back", "4", "") // choose, done with nothing -> refused, back, none, Solo
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "Nothing is ticked. Tick at least one session, or type back.", "Sharing: share no tmux session.")
	if got := r.per.saved(); len(got) != 1 || got[0].Share.Mode != store.ShareNone {
		t.Errorf("saved = %+v", got)
	}
}

func TestTerminalChooseNowPreTicksTheSavedSelection(t *testing.T) {
	r := newRig([]string{"build", "docs"}, Provided{})
	r.per.current = Saved{Share: store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"docs"}, Configured: true}}
	u := newTypist(t)
	u.typeLines("", "", "") // default (choose, pre-ticked docs) -> Enter keeps it; Hub: Enter = Solo
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "Your choice [2]:", "[x] 2  docs")
	if got := r.per.saved(); len(got) != 1 || strings.Join(got[0].Share.Sessions, ",") != "docs" {
		t.Errorf("saved = %+v", got)
	}
}

func TestTerminalNoSessionsToChoose(t *testing.T) {
	r := newRig(nil, Provided{})
	u := newTypist(t)
	u.typeLines("2", "3", "") // choose: nothing to choose from; later; Solo
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "I see no tmux sessions right now.", "There are no sessions to choose from. Pick another option.")
	if got := r.per.saved(); len(got) != 1 || got[0].Share.Mode != store.ShareLater {
		t.Errorf("saved = %+v", got)
	}
}

// "Configure later from the Hub" saves the later mode (nothing is shared) and
// says so honestly: no claim that the Hub already offers the page.
func TestTerminalLaterFailsClosedAndSaysSo(t *testing.T) {
	r := newRig([]string{"a", "b"}, Provided{})
	u := newTypist(t)
	u.typeLines("later", "")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "Nothing is shared until you choose.", "minion worker config share")
	lacks(t, strings.ToLower(o.out), "already configured", "configured from the hub")
	got := r.per.saved()
	if len(got) != 1 || got[0].Share.Mode != store.ShareLater || got[0].Share.Mode.Valid() == false {
		t.Fatalf("saved = %+v", got)
	}
	if got[0].Share.Includes("a") || got[0].Share.Includes("b") {
		t.Error("later must share nothing")
	}
}

func TestTerminalInvalidAnswersAreAskedAgain(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	u := newTypist(t)
	u.typeLines("7", "banana", "none", "ftp://x", "", "")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	if n := strings.Count(o.out, "Please type 1, 2, 3 or 4."); n != 2 {
		t.Errorf("share question re-asked %d times, want 2:\n%s", n, o.out)
	}
	contains(t, o.out, "must start with https://")
}

func TestTerminalFailedCheckCorrectOrSaveAnyway(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	r.chk.reports["https://bad.example:1"] = failedReport("")
	u := newTypist(t)
	// share all; a bad address; "c" to correct; a good address; pairing waits
	u.typeLines("1", "https://bad.example:1", "c", "https://hub.example:8443")
	r.pair.wait = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, done := startDialog(t, ctx, r.w, u.r)
	<-r.pair.started
	u.press(t, "") // Enter: finish without waiting
	o := waitDone(t, done)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	_ = out
	contains(t, o.out,
		"FAILED",
		"nothing listens on port 1",
		"skipped",
		"Type c to correct the address, or s to save it anyway [c]:",
		"Pairing skipped. Run `minion worker pair`",
	)
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "https://hub.example:8443" {
		t.Errorf("saved = %+v; the corrected address must be the one that is saved", got)
	}
	if got := r.chk.called(); len(got) != 2 {
		t.Errorf("checked = %v", got)
	}
	if o.res.Pairing.Paired {
		t.Error("skipping the wait must not claim a pairing")
	}
}

func TestTerminalSaveAnywayWithAFailedCheck(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	r.chk.reports["https://bad.example:1"] = failedReport("")
	u := newTypist(t)
	u.typeLines("1", "https://bad.example:1", "x", "s") // "x" is neither c nor s
	r.pair.wait = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, done := startDialog(t, ctx, r.w, u.r)
	<-r.pair.started
	u.press(t, "")
	o := waitDone(t, done)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "Please type c or s.", "Saved anyway.")
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "https://bad.example:1" {
		t.Errorf("saved = %+v", got)
	}
	if calls := r.pair.calls(); len(calls) != 1 || calls[0] != "https://bad.example:1" {
		t.Errorf("pairing calls = %v", calls)
	}
}

// A self-signed Hub is a warning, not a failure: the user is not asked to
// correct anything, and the fingerprint is on the screen.
func TestTerminalSelfSignedHubShowsTheFingerprintWithoutAQuestion(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	r.chk.reports["https://hub.example:8443"] = selfSignedReport("")
	u := newTypist(t)
	u.typeLines("1", "https://hub.example:8443")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "WARNING", "not trusted by this computer", "SHA-256 AA:BB")
	lacks(t, o.out, "Type c to correct")
}

func TestTerminalPairingWaitsAndEndsWithPairedTo(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	u := newTypist(t)
	u.typeLines("1", "https://hub.example:8443")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	idx := strings.LastIndex(o.out, "paired to alice")
	if idx < 0 {
		t.Fatalf("no \"paired to alice\" in:\n%s", o.out)
	}
	if !strings.Contains(o.out[:idx], "Pairing code: MN-K7Q4-XY12") {
		t.Error("the code must be shown before the pairing completes")
	}
}

func TestTerminalAlreadyPaired(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	r.pair.already = &Paired{Owner: "bob"}
	u := newTypist(t)
	u.typeLines("1", "https://hub.example:8443")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "This Worker is already paired to bob.", "Pairing: paired to bob")
	lacks(t, o.out, "Pairing code:")
	if len(r.pair.calls()) != 0 {
		t.Error("no code may be offered to a paired Worker")
	}
}

// Ctrl-D, a closed input or Ctrl-C before the settings are saved change nothing.
func TestTerminalEndOfInputBeforeSaveChangesNothing(t *testing.T) {
	for name, lines := range map[string][]string{
		"in the share step": nil,
		"in the Hub step":   {"1"},
	} {
		t.Run(name, func(t *testing.T) {
			rr := newRig([]string{"a"}, Provided{})
			o := runDialog(t, rr.w, strings.NewReader(strings.Join(lines, "\n")+"\n"), true)
			if !errors.Is(o.err, ErrCancelled) {
				t.Errorf("err = %v, want ErrCancelled", o.err)
			}
			if got := rr.per.saved(); len(got) != 0 {
				t.Errorf("a cancelled wizard saved %+v", got)
			}
		})
	}
}

func TestTerminalCancelledContextBeforeSaveChangesNothing(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	u := newTypist(t) // never answers
	ctx, cancel := context.WithCancel(context.Background())
	out, done := startDialog(t, ctx, r.w, u.r)
	out.waitFor(t, "Your choice") // the question is on the screen
	cancel()
	o := waitDone(t, done)
	if !errors.Is(o.err, ErrCancelled) {
		t.Errorf("err = %v", o.err)
	}
	if got := r.per.saved(); len(got) != 0 {
		t.Errorf("saved = %+v", got)
	}
}

// Ctrl-C while waiting for the Hub, after the save: the settings stay and the
// error is the context's.
func TestTerminalCancelWhilePairingKeepsTheSavedSettings(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	r.pair.wait = true
	u := newTypist(t)
	u.typeLines("1", "https://hub.example:8443")
	ctx, cancel := context.WithCancel(context.Background())
	_, done := startDialog(t, ctx, r.w, u.r)
	<-r.pair.started
	cancel()
	o := waitDone(t, done)
	if !errors.Is(o.err, context.Canceled) {
		t.Errorf("err = %v", o.err)
	}
	contains(t, o.out, "Stopped. Your settings are saved.")
	if got := r.per.saved(); len(got) != 1 {
		t.Errorf("saved = %+v", got)
	}
}

func TestTerminalSaveFailureIsAnError(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	r.per.saveErr = errors.New("disk full")
	u := newTypist(t)
	u.typeLines("1", "")
	o := runDialog(t, r.w, u.r, true)
	if o.err == nil || !strings.Contains(o.err.Error(), "could not save the settings") || !strings.Contains(o.err.Error(), "disk full") {
		t.Fatalf("err = %v", o.err)
	}
	lacks(t, o.out, "Your choices are saved.")
	if len(r.pair.calls()) != 0 {
		t.Error("pairing started after a failed save")
	}
}

// R85: everything given as a flag or environment variable means no question.
// The input is never read, in a terminal or not.
func TestTerminalAllValuesProvidedAsksNothingAndReadsNothing(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		name := map[bool]string{false: "no terminal", true: "terminal"}[interactive]
		t.Run(name, func(t *testing.T) {
			prov := Provided{Share: store.ShareAll, Server: "https://hub.example:8443", HasServer: true}
			r := newRig([]string{"a", "b"}, prov)
			in := &countingReader{t: t}
			if interactive {
				// In a terminal the pairing step waits for Enter; that is the
				// only read, and it is not a question.
				u := newTypist(t)
				r.pair.wait = true
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				_, done := startDialog(t, ctx, r.w, u.r)
				<-r.pair.started
				u.press(t, "")
				o := waitDone(t, done)
				if o.err != nil {
					t.Fatalf("err = %v\n%s", o.err, o.out)
				}
				lacks(t, o.out, "Your choice [", "Hub address:", "Type c to correct")
				contains(t, o.out, "Sharing: share all tmux sessions", "given as a setting, so nothing is asked")
			} else {
				o := runDialog(t, r.w, in, false)
				if o.err != nil {
					t.Fatalf("err = %v\n%s", o.err, o.out)
				}
				lacks(t, o.out, "Your choice [", "Hub address:")
				contains(t, o.out, "Not pairing now, because there is no terminal")
				if len(r.pair.calls()) != 0 {
					t.Error("without a terminal the wizard must not start pairing")
				}
			}
			got := r.per.saved()
			if len(got) != 1 || got[0].Share.Mode != store.ShareAll || got[0].Server != "https://hub.example:8443" {
				t.Errorf("saved = %+v", got)
			}
		})
	}
}

func TestTerminalProvidedSoloAsksNothing(t *testing.T) {
	r := newRig([]string{"a"}, Provided{Share: store.ShareNone, HasServer: true})
	o := runDialog(t, r.w, &countingReader{t: t}, false)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "Hub: none, the Worker runs alone (Solo).", "No Hub, so there is nothing to pair with")
	got := r.per.saved()
	if len(got) != 1 || got[0].Server != "" || got[0].Share.Mode != store.ShareNone {
		t.Errorf("saved = %+v", got)
	}
}

// A provided address that fails the check is still saved (the user said so
// explicitly), with a pointer to the fix. An address that is not valid at all
// is an error and nothing is saved.
func TestTerminalProvidedAddressFailureIsReportedAndSaved(t *testing.T) {
	r := newRig(nil, Provided{Share: store.ShareAll, Server: "https://bad.example:1", HasServer: true})
	r.chk.reports["https://bad.example:1"] = failedReport("")
	o := runDialog(t, r.w, &countingReader{t: t}, false)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "FAILED", "saved as it is", "minion worker config set server")
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "https://bad.example:1" {
		t.Errorf("saved = %+v", got)
	}
}

func TestTerminalProvidedInvalidAddressIsAnError(t *testing.T) {
	r := newRig(nil, Provided{Share: store.ShareAll, Server: "ftp://hub", HasServer: true})
	o := runDialog(t, r.w, &countingReader{t: t}, false)
	if o.err == nil || !strings.Contains(o.err.Error(), "cannot be used") {
		t.Fatalf("err = %v", o.err)
	}
	if len(r.per.saved()) != 0 {
		t.Error("an invalid address must not be saved")
	}
}

// Without a terminal and without the values, the wizard must not read input,
// must not save and must not start pairing.
func TestTerminalWithoutATerminalNeedsTheValues(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	o := runDialog(t, r.w, &countingReader{t: t}, false)
	if !errors.Is(o.err, ErrNeedsTerminal) {
		t.Fatalf("err = %v, want ErrNeedsTerminal", o.err)
	}
	if len(r.per.saved()) != 0 || len(r.pair.calls()) != 0 {
		t.Error("something happened without a terminal")
	}
}

// With the share choice given but no Hub address and no terminal, the Worker
// runs alone: nothing is asked.
func TestTerminalNoTerminalAndNoAddressMeansSolo(t *testing.T) {
	r := newRig([]string{"a"}, Provided{Share: store.ShareLater})
	o := runDialog(t, r.w, &countingReader{t: t}, false)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "" || got[0].Share.Mode != store.ShareLater {
		t.Errorf("saved = %+v", got)
	}
}

// With no terminal, a Hub address that was saved earlier is kept and checked.
func TestTerminalNoTerminalKeepsTheSavedAddress(t *testing.T) {
	r := newRig([]string{"a"}, Provided{Share: store.ShareAll})
	r.per.current = Saved{Server: "https://hub.example:8443"}
	o := runDialog(t, r.w, &countingReader{t: t}, false)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "https://hub.example:8443" {
		t.Errorf("saved = %+v", got)
	}
}

func TestTerminalDefaultsToTheSavedHubAndSoloWordClearsIt(t *testing.T) {
	r := newRig([]string{"a"}, Provided{})
	r.per.current = Saved{Server: "https://old.example:8443", Share: store.ShareSelection{Mode: store.ShareAll, Configured: true}}
	u := newTypist(t)
	u.typeLines("1", "solo")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "Hub address [https://old.example:8443] (type solo to run without a Hub):")
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "" {
		t.Errorf("saved = %+v; solo must clear the saved address", got)
	}
}

// A hostile session name must not reach the terminal as an escape sequence.
func TestTerminalEscapesHostileSessionNames(t *testing.T) {
	odd := "evil\x1b]0;pwned\x07name"
	r := newRig([]string{odd, "ok"}, Provided{})
	u := newTypist(t)
	u.typeLines("2", "1", "", "")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	if strings.ContainsAny(o.out, "\x1b\x07") {
		t.Errorf("the dialog printed a control character:\n%q", o.out)
	}
	got := r.per.saved()
	if len(got) != 1 || got[0].Share.Sessions[0] != odd {
		t.Errorf("the exact name must be saved: %+v", got)
	}
}

func TestTerminalListsAtMostTenNames(t *testing.T) {
	var names []string
	for i := 0; i < 14; i++ {
		names = append(names, "s"+string(rune('a'+i)))
	}
	r := newRig(names, Provided{Share: store.ShareNone})
	o := runDialog(t, r.w, &countingReader{t: t}, false)
	if o.err != nil {
		t.Fatalf("err = %v", o.err)
	}
	contains(t, o.out, "I see 14 tmux sessions:", "and 4 more")
}

func TestTerminalUnlistableSessionsStillAllowsAll(t *testing.T) {
	r := newRig(nil, Provided{})
	r.w = New(Config{Sessions: fakeLister{err: errors.New("tmux is not installed")}, Persist: r.per, Check: r.chk, Pairing: r.pair})
	u := newTypist(t)
	u.typeLines("2", "1", "")
	o := runDialog(t, r.w, u.r, true)
	if o.err != nil {
		t.Fatalf("err = %v\n%s", o.err, o.out)
	}
	contains(t, o.out, "I could not list your tmux sessions (tmux is not installed).", "There are no sessions to choose from.")
}

func TestWrapAfterKeepsAlignment(t *testing.T) {
	got := wrapAfter("  DNS       OK       ", strings.Repeat("word ", 30))
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("not wrapped: %q", got)
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, strings.Repeat(" ", 21)) {
			t.Errorf("continuation line not indented: %q", l)
		}
	}
	for _, l := range lines {
		if len([]rune(l)) > 80 {
			t.Errorf("line too long: %q", l)
		}
	}
}

// With an invitation key the Worker enrolls by itself: no code is offered.
func TestTerminalSkipPairingExplainsInsteadOfOfferingACode(t *testing.T) {
	r := newRig([]string{"a"}, Provided{Share: store.ShareAll, Server: "https://hub.example:8443", HasServer: true})
	out := &syncBuffer{}
	term := &Terminal{In: &countingReader{t: t}, Out: out, Interactive: true, SkipPairing: "An invitation key was given; no pairing code is needed."}
	res, err := term.Run(context.Background(), r.w)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	contains(t, out.String(), "An invitation key was given; no pairing code is needed.")
	lacks(t, out.String(), "Pairing code:")
	if res.Pairing.Skipped != "invitation" || len(r.pair.calls()) != 0 {
		t.Errorf("pairing = %+v calls %v", res.Pairing, r.pair.calls())
	}
}
