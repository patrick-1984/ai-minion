package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// ErrNeedsTerminal is returned when a question has to be asked and there is
// no terminal to ask it on. Nothing was read and nothing was changed.
var ErrNeedsTerminal = errors.New("setup needs to ask a question, but there is no terminal")

// maxListedSessions bounds how many session names step 1 prints.
const maxListedSessions = 10

// Terminal runs the [Wizard] as a line-based dialog. All text is English.
type Terminal struct {
	// In is where answers are read from. It is read only when a question has
	// to be asked (or to wait for Enter during pairing), and only when
	// Interactive is true.
	In io.Reader
	// Out receives the dialog.
	Out io.Writer
	// Interactive says that In and Out are a terminal, so that questions may
	// be asked. When false the dialog asks nothing, reads nothing and fails
	// with [ErrNeedsTerminal] if a value is missing.
	Interactive bool
	// SkipPairing, when not empty, replaces step 3: the text is shown and no
	// code is offered. "worker up" uses it when an invitation key is given,
	// because that key enrolls the Worker by itself.
	SkipPairing string
}

// Result is what a finished terminal wizard reports.
type Result struct {
	Summary Summary
	Pairing PairOutcome
}

// Run runs the three steps. When ctx ends before the settings are saved it
// returns [ErrCancelled]; after they are saved it returns ctx.Err() and the
// settings stay. Answers that flags or environment already provide are not
// asked.
func (t *Terminal) Run(ctx context.Context, w *Wizard) (Result, error) {
	rd := &lineReader{in: t.In}
	d := dialog{t: t, w: w, rd: rd}

	if err := d.shareStep(ctx); err != nil {
		return Result{}, err
	}
	if err := d.hubStep(ctx); err != nil {
		return Result{}, err
	}
	if err := w.Save(ctx); err != nil {
		return Result{}, fmt.Errorf("could not save the settings: %w", err)
	}
	sum, _ := w.Summary()
	d.say("")
	d.say("Your choices are saved.")

	res := Result{Summary: sum}
	out, err := d.pairStep(ctx)
	res.Pairing = out
	if err != nil {
		return res, err
	}
	d.finish(res)
	return res, nil
}

type dialog struct {
	t  *Terminal
	w  *Wizard
	rd *lineReader
}

func (d *dialog) say(format string, a ...any) {
	if len(a) == 0 {
		_, _ = fmt.Fprintln(d.t.Out, format)
		return
	}
	_, _ = fmt.Fprintf(d.t.Out, format+"\n", a...)
}

// ask prints prompt and reads one answer.
func (d *dialog) ask(ctx context.Context, prompt string) (string, error) {
	if !d.t.Interactive {
		return "", ErrNeedsTerminal
	}
	_, _ = fmt.Fprint(d.t.Out, prompt)
	line, err := d.rd.next(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(d.t.Out)
		return "", ErrCancelled
	}
	return strings.TrimSpace(line), nil
}

// ---- step 1 ----

func (d *dialog) shareStep(ctx context.Context) error {
	v, err := d.w.ShareStep(ctx)
	if err != nil {
		return err
	}
	d.say("")
	d.say("Step 1 of 3: What may the Hub see?")
	if !v.Listed {
		d.say("I could not list your tmux sessions (%s).", oneLine(v.ListError))
	} else {
		d.say("%s", seeLine(v.Sessions))
		if len(v.Sessions) > 0 {
			shown := v.Sessions
			if len(shown) > maxListedSessions {
				shown = shown[:maxListedSessions]
			}
			names := make([]string, len(shown))
			for i, n := range shown {
				names[i] = DisplayName(n)
			}
			line := "  " + strings.Join(names, ", ")
			if len(v.Sessions) > len(shown) {
				line += fmt.Sprintf(", and %d more", len(v.Sessions)-len(shown))
			}
			d.say("%s", line)
		}
	}
	if v.Decided != "" {
		if err := d.w.ApplyProvided(ctx); err != nil {
			return fmt.Errorf("the sharing choice you gave (%s) cannot be used: %w", v.Decided, err)
		}
		d.say("Sharing: %s (given as a setting, so nothing is asked).", DescribeShare(store.ShareSelection{Mode: v.Decided}))
		return nil
	}

	def := "1"
	switch v.Default.Mode {
	case store.ShareSelect:
		def = "2"
	case store.ShareLater:
		def = "3"
	case store.ShareNone:
		def = "4"
	}
	d.say("  1) all     Share all of them, also sessions you create later (default)")
	d.say("  2) choose  Choose now which sessions to share")
	d.say("  3) later   Configure later from the Hub; nothing is shared until you do")
	d.say("  4) none    Share no session")
	d.say("Sessions are never renamed, closed or changed by this choice.")
	for {
		ans, err := d.ask(ctx, fmt.Sprintf("Your choice [%s]: ", def))
		if err != nil {
			return err
		}
		if ans == "" {
			ans = def
		}
		switch strings.ToLower(ans) {
		case "1", "all", "a":
			return d.choose(ctx, store.ShareSelection{Mode: store.ShareAll})
		case "3", "later", "l":
			if err := d.choose(ctx, store.ShareSelection{Mode: store.ShareLater}); err != nil {
				return err
			}
			d.say("Nothing is shared until you choose. Until the Hub can do this for you, run:")
			d.say("  minion worker config share all    (or: none, or: select <session>...)")
			return nil
		case "4", "none", "n":
			return d.choose(ctx, store.ShareSelection{Mode: store.ShareNone})
		case "2", "choose", "c":
			sel, ok, err := d.tick(ctx, v)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			return d.choose(ctx, sel)
		}
		d.say("Please type 1, 2, 3 or 4.")
	}
}

func (d *dialog) choose(ctx context.Context, sel store.ShareSelection) error {
	if err := d.w.ChooseShare(ctx, sel); err != nil {
		return err
	}
	d.say("Sharing: %s.", DescribeShare(sel))
	return nil
}

func seeLine(sessions []string) string {
	switch len(sessions) {
	case 0:
		return "I see no tmux sessions right now."
	case 1:
		return "I see 1 tmux session:"
	}
	return fmt.Sprintf("I see %d tmux sessions:", len(sessions))
}

// tick shows the checkbox list and returns the ticked sessions. ok is false
// when the user went back to the options.
func (d *dialog) tick(ctx context.Context, v ShareView) (sel store.ShareSelection, ok bool, err error) {
	if !v.Listed || len(v.Sessions) == 0 {
		d.say("There are no sessions to choose from. Pick another option.")
		return sel, false, nil
	}
	ticked := make([]bool, len(v.Sessions))
	pre := map[string]bool{}
	for _, n := range v.Default.Sessions {
		pre[n] = true
	}
	for i, n := range v.Sessions {
		ticked[i] = pre[n]
	}
	for {
		d.say("")
		d.say("Type a number to tick or untick a session (several numbers are fine).")
		d.say("Press Enter when you are done, or type back to return to the options.")
		for i, n := range v.Sessions {
			mark := " "
			if ticked[i] {
				mark = "x"
			}
			d.say("  [%s] %d  %s", mark, i+1, DisplayName(n))
		}
		ans, err := d.ask(ctx, "> ")
		if err != nil {
			return sel, false, err
		}
		switch strings.ToLower(ans) {
		case "back", "b":
			return sel, false, nil
		case "":
			var names []string
			for i, n := range v.Sessions {
				if ticked[i] {
					names = append(names, n)
				}
			}
			if len(names) == 0 {
				d.say("Nothing is ticked. Tick at least one session, or type back.")
				continue
			}
			return store.ShareSelection{Mode: store.ShareSelect, Sessions: names}, true, nil
		}
		bad := ""
		for _, f := range strings.FieldsFunc(ans, func(r rune) bool { return r == ' ' || r == ',' }) {
			n, err := strconv.Atoi(f)
			if err != nil || n < 1 || n > len(v.Sessions) {
				bad = f
				continue
			}
			ticked[n-1] = !ticked[n-1]
		}
		if bad != "" {
			d.say("%q is not a number from the list.", DisplayName(bad))
		}
	}
}

// ---- step 2 ----

func (d *dialog) hubStep(ctx context.Context) error {
	v, err := d.w.HubStep(ctx)
	if err != nil {
		return err
	}
	d.say("")
	d.say("Step 2 of 3: Hub address")
	decided, given := v.Decided, "given as a setting"
	if !decided && !d.t.Interactive {
		// Nothing can be asked: keep the saved address, or run alone.
		v.Decided, v.Server, decided, given = true, v.Default, true, "not given and there is no terminal to ask"
	}
	if decided {
		if v.Server == "" {
			if err := d.w.SkipHub(); err != nil {
				return err
			}
			d.say("Hub: none, the Worker runs alone (Solo). The address was %s, so nothing is asked.", given)
			return nil
		}
		rep, err := d.w.CheckHub(ctx, v.Server)
		if err != nil {
			return fmt.Errorf("the Hub address you gave cannot be used: %w", err)
		}
		d.report(rep)
		if rep.Failed() {
			d.say("The address was %s, so it is saved as it is. Fix it with: minion worker config set server <address>", given)
		}
		return d.w.UseHub()
	}

	d.say("Type the address of your Minion Hub, for example https://hub.example:8443")
	d.say("Press Enter to skip: the Worker then runs alone on this computer (Solo).")
	for {
		prompt := "Hub address: "
		if v.Default != "" {
			prompt = fmt.Sprintf("Hub address [%s] (type solo to run without a Hub): ", redact(v.Default))
		}
		ans, err := d.ask(ctx, prompt)
		if err != nil {
			return err
		}
		switch {
		case ans == "" && v.Default != "":
			ans = v.Default
		case ans == "" || strings.EqualFold(ans, "solo") || strings.EqualFold(ans, "skip"):
			d.say("No Hub: the Worker runs alone (Solo). Add one later with: minion worker config set server <address>")
			return d.w.SkipHub()
		}
		if !strings.Contains(ans, "://") {
			ans = "https://" + ans
			d.say("(I added https:// in front: %s)", redact(ans))
		}
		rep, err := d.w.CheckHub(ctx, ans)
		if errors.Is(err, ErrInvalidChoice) {
			d.say("%s", strings.TrimPrefix(err.Error(), ErrInvalidChoice.Error()+": "))
			continue
		}
		if err != nil {
			return err
		}
		d.report(rep)
		if !rep.Failed() {
			return d.w.UseHub()
		}
		for {
			a, err := d.ask(ctx, "Type c to correct the address, or s to save it anyway [c]: ")
			if err != nil {
				return err
			}
			switch strings.ToLower(a) {
			case "", "c", "correct":
			case "s", "save":
				d.say("Saved anyway. You can change it later with: minion worker config set server <address>")
				return d.w.UseHub()
			default:
				d.say("Please type c or s.")
				continue
			}
			break
		}
	}
}

// report prints the stages of a check.
func (d *dialog) report(rep Report) {
	d.say("Checking %s", rep.URL)
	for _, n := range rep.Notes {
		d.say("%s", wrapAfter("  Note: ", n))
	}
	for _, s := range rep.Stages {
		tag := map[Status]string{StatusOK: "OK", StatusWarning: "WARNING", StatusFailed: "FAILED", StatusSkipped: "skipped"}[s.Status]
		d.say("%s", wrapAfter(fmt.Sprintf("  %-9s %-8s ", s.Name, tag), s.Message))
	}
}

// ---- step 3 ----

func (d *dialog) pairStep(ctx context.Context) (PairOutcome, error) {
	sum, _ := d.w.Summary()
	d.say("")
	d.say("Step 3 of 3: Pairing")
	if sum.Server == "" {
		d.say("No Hub, so there is nothing to pair with. The Worker runs alone (Solo).")
		return PairOutcome{Skipped: "solo"}, nil
	}
	if d.t.SkipPairing != "" {
		d.say("%s", wrapAfter("", d.t.SkipPairing))
		return PairOutcome{Skipped: "invitation"}, nil
	}
	if !d.t.Interactive {
		d.say("Not pairing now, because there is no terminal to show the code on.")
		d.say("Run `minion worker pair` in a terminal when you want a pairing code.")
		return PairOutcome{Skipped: "no terminal"}, nil
	}
	d.say("The pairing code will appear here when the Hub is reachable. Type it into the Hub")
	d.say("under \"Add worker\". Press Enter to finish without waiting.")

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// Enter (or Ctrl-D) ends the wait; the wizard itself stays alive.
		if _, err := d.rd.next(pctx); err == nil || errors.Is(err, io.EOF) {
			cancel()
		}
	}()

	tw := &trackWriter{w: d.t.Out}
	out, err := d.w.Pair(pctx, tw)
	cancel()
	if err != nil {
		return out, fmt.Errorf("pairing failed: %w", err)
	}
	if ctx.Err() != nil {
		d.say("")
		d.say("Stopped. Your settings are saved. Run `minion worker pair` to get a pairing code later.")
		return out, ctx.Err()
	}
	switch {
	case out.Paired && out.Already:
		d.say("This Worker is already paired to %s.", out.Owner)
	case out.Paired:
		if !tw.sawPaired() {
			d.say("paired to %s", out.Owner)
		}
	case out.Skipped == "unavailable":
		d.say("Pairing is not available here. Run `minion worker pair` to get a pairing code.")
	default:
		d.say("Pairing skipped. Run `minion worker pair` whenever you want a pairing code.")
	}
	return out, nil
}

func (d *dialog) finish(res Result) {
	d.say("")
	d.say("Setup is done.")
	d.say("  Sharing: %s", DescribeShare(res.Summary.Share))
	if res.Summary.Server == "" {
		d.say("  Hub:     none (Solo)")
	} else {
		d.say("  Hub:     %s", redact(res.Summary.Server))
	}
	switch {
	case res.Pairing.Paired:
		d.say("  Pairing: paired to %s", res.Pairing.Owner)
	case res.Summary.Server != "":
		d.say("  Pairing: not paired yet (run `minion worker pair`)")
	}
}

// trackWriter forwards to w and remembers whether the pairing flow already
// printed its "paired to <user>" line, so that the dialog never prints it twice.
type trackWriter struct {
	mu   sync.Mutex
	w    io.Writer
	seen bool
}

func (t *trackWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if strings.Contains(string(p), "paired to ") {
		t.seen = true
	}
	return t.w.Write(p)
}

func (t *trackWriter) sawPaired() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen
}

// lineReader reads one line from in per question, on a goroutine that starts
// at that question, so that no input is read when nothing is asked, and so
// that a blocked read never keeps a cancelled wizard from returning.
//
// It reads byte by byte and stops at the newline: it never reads ahead and
// never keeps a goroutine that reads on its own. Whatever follows the answered
// line stays unread in in, so a later consumer of the same stream (for example
// "--invite-key -", which takes the next line of stdin for the enrollment
// hook) still receives it. The only read that can be left behind is the one
// a cancelled question was blocked in; it is handed to the next question.
type lineReader struct {
	in      io.Reader
	mu      sync.Mutex // serializes next
	pending chan lineResult
}

type lineResult struct {
	line string
	err  error
}

// readLine reads up to and including the next newline without reading past it.
func readLine(in io.Reader) lineResult {
	if in == nil {
		return lineResult{err: io.EOF}
	}
	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := in.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return lineResult{line: strings.TrimRight(string(buf), "\r")}
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return lineResult{line: strings.TrimRight(string(buf), "\r")}
			}
			return lineResult{err: err}
		}
	}
}

// next returns the next line, or ctx.Err() when ctx ends first, or the read
// error (io.EOF at the end of the input).
func (r *lineReader) next(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		ch := make(chan lineResult, 1)
		r.pending = ch
		go func() { ch <- readLine(r.in) }()
	}
	select {
	case res := <-r.pending:
		r.pending = nil
		return res.line, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 120 {
		s = string([]rune(s)[:119]) + "…"
	}
	return s
}

// wrapAfter returns prefix followed by text, broken at about 78 columns;
// continuation lines are indented to the end of prefix.
func wrapAfter(prefix, text string) string {
	const width = 78
	indent := strings.Repeat(" ", utf8.RuneCountInString(prefix))
	var b strings.Builder
	first := true
	cur := prefix
	for _, w := range strings.Fields(text) {
		if !first && utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) > width {
			b.WriteString(strings.TrimRight(cur, " ") + "\n")
			cur = indent + w
			continue
		}
		if first {
			cur += w
			first = false
		} else {
			cur += " " + w
		}
	}
	b.WriteString(cur)
	return b.String()
}
