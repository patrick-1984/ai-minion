package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// ---- fakes ----

type fakeLister struct {
	names []string
	err   error
}

func (f fakeLister) ListSessions(context.Context) ([]term.Session, error) {
	var out []term.Session
	for _, n := range f.names {
		out = append(out, term.Session{Name: n})
	}
	return out, f.err
}

// fakePersister records what is saved.
type fakePersister struct {
	mu      sync.Mutex
	current Saved
	saves   []Settings
	saveErr error
}

func (p *fakePersister) Current(context.Context) (Saved, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current, nil
}

func (p *fakePersister) Save(_ context.Context, s Settings) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saveErr != nil {
		return p.saveErr
	}
	p.saves = append(p.saves, s)
	return nil
}

func (p *fakePersister) saved() []Settings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Settings(nil), p.saves...)
}

// fakeChecker answers per address and records the calls.
type fakeChecker struct {
	mu      sync.Mutex
	reports map[string]Report
	calls   []string
}

func (c *fakeChecker) Check(_ context.Context, hubURL string) Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, hubURL)
	if r, ok := c.reports[hubURL]; ok {
		r.URL = hubURL
		return r
	}
	return okReport(hubURL)
}

func (c *fakeChecker) called() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func okReport(u string) Report {
	return Report{URL: u, Stages: []Stage{
		{StageDNS, StatusOK, "hub.example was found (203.0.113.7)."},
		{StageTCP, StatusOK, "Connected to hub.example on port 8443."},
		{StageTLS, StatusOK, "The Hub's certificate is trusted by this computer."},
		{StageProtocol, StatusOK, "The Hub speaks the Minion connection protocol."},
	}}
}

func failedReport(u string) Report {
	return Report{URL: u, Stages: []Stage{
		{StageDNS, StatusOK, "hub.example was found (203.0.113.7)."},
		{StageTCP, StatusFailed, "hub.example answered, but nothing listens on port 1 (connection refused). Check the port number in the address."},
		{StageTLS, StatusSkipped, "Not checked, because an earlier step failed."},
		{StageProtocol, StatusSkipped, "Not checked, because an earlier step failed."},
	}}
}

func selfSignedReport(u string) Report {
	r := okReport(u)
	r.Stages[2] = Stage{StageTLS, StatusWarning, "The Hub's certificate is not trusted by this computer: it is signed by an authority this computer does not know (self-signed or private authority). Certificate fingerprint SHA-256 AA:BB."}
	r.CertFingerprint = "SHA-256 AA:BB"
	return r
}

// fakePairing is the pairing port. With wait set it prints a code and holds
// until its context ends; otherwise it pairs at once.
type fakePairing struct {
	mu       sync.Mutex
	already  *Paired
	owner    string
	wait     bool
	err      error
	started  chan struct{}
	pairCall []string
}

func newFakePairing() *fakePairing {
	return &fakePairing{owner: "alice", started: make(chan struct{}, 8)}
}

func (p *fakePairing) Paired(context.Context) (Paired, bool, error) {
	if p.already != nil {
		return *p.already, true, nil
	}
	return Paired{}, false, nil
}

func (p *fakePairing) Pair(ctx context.Context, hubURL string, out io.Writer) (Paired, error) {
	p.mu.Lock()
	p.pairCall = append(p.pairCall, hubURL)
	p.mu.Unlock()
	_, _ = fmt.Fprintf(out, "Pairing code: MN-K7Q4-XY12\nEnter this code in the Hub (Add worker) within 10 minutes. Hub: %s\n", hubURL)
	p.started <- struct{}{}
	if p.err != nil {
		return Paired{}, p.err
	}
	if p.wait {
		<-ctx.Done()
		return Paired{}, ctx.Err()
	}
	_, _ = fmt.Fprintf(out, "paired to %s\n", p.owner)
	return Paired{Owner: p.owner}, nil
}

func (p *fakePairing) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pairCall...)
}

type rig struct {
	w    *Wizard
	per  *fakePersister
	chk  *fakeChecker
	pair *fakePairing
}

func newRig(names []string, prov Provided) *rig {
	r := &rig{per: &fakePersister{}, chk: &fakeChecker{reports: map[string]Report{}}, pair: newFakePairing()}
	r.w = New(Config{Sessions: fakeLister{names: names}, Persist: r.per, Check: r.chk, Pairing: r.pair, Provided: prov})
	return r
}

// ---- engine tests ----

func TestStepsAreEnforcedInOrder(t *testing.T) {
	ctx := context.Background()
	r := newRig([]string{"a"}, Provided{})
	if _, err := r.w.CheckHub(ctx, "https://hub.example"); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("CheckHub before the share choice: %v", err)
	}
	if _, err := r.w.HubStep(ctx); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("HubStep before the share choice: %v", err)
	}
	if err := r.w.SkipHub(); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("SkipHub before the share choice: %v", err)
	}
	if err := r.w.Save(ctx); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("Save before anything: %v", err)
	}
	if err := r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	if err := r.w.UseHub(); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("UseHub without a check: %v", err)
	}
	if err := r.w.Save(ctx); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("Save before the Hub step: %v", err)
	}
	if _, err := r.w.Pair(ctx, io.Discard); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("Pair before Save: %v", err)
	}
	if len(r.per.saved()) != 0 {
		t.Errorf("something was saved before the steps were done: %+v", r.per.saved())
	}
}

func TestShareStepListsSessionsAndDefaultsToAll(t *testing.T) {
	r := newRig([]string{"build", "docs"}, Provided{})
	v, err := r.w.ShareStep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !v.Listed || strings.Join(v.Sessions, ",") != "build,docs" {
		t.Errorf("view: %+v", v)
	}
	if v.Default.Mode != store.ShareAll {
		t.Errorf("a first run offers all by default, got %q", v.Default.Mode)
	}
}

func TestShareStepOffersTheSavedChoiceAsDefault(t *testing.T) {
	r := newRig([]string{"build", "docs"}, Provided{})
	r.per.current = Saved{Share: store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"docs", "gone"}, Configured: true}}
	v, _ := r.w.ShareStep(context.Background())
	if v.Default.Mode != store.ShareSelect || strings.Join(v.Default.Sessions, ",") != "docs" {
		t.Errorf("default = %+v; a session that no longer exists must not be pre-ticked", v.Default)
	}
}

func TestChooseShareSelectValidation(t *testing.T) {
	ctx := context.Background()
	r := newRig([]string{"build", "docs"}, Provided{})
	for name, sel := range map[string]store.ShareSelection{
		"nothing chosen":  {Mode: store.ShareSelect},
		"unknown session": {Mode: store.ShareSelect, Sessions: []string{"nope"}},
		"unknown mode":    {Mode: "sometimes"},
	} {
		if err := r.w.ChooseShare(ctx, sel); !errors.Is(err, ErrInvalidChoice) {
			t.Errorf("%s: err = %v, want ErrInvalidChoice", name, err)
		}
	}
	if err := r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"docs", "docs"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.w.SkipHub(); err != nil {
		t.Fatal(err)
	}
	sum, err := r.w.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sum.Share.Sessions, ",") != "docs" {
		t.Errorf("duplicates must collapse: %v", sum.Share.Sessions)
	}
}

func TestChooseShareSelectNeedsListableSessions(t *testing.T) {
	r := New(Config{Sessions: fakeLister{err: errors.New("tmux is not installed")}, Persist: &fakePersister{}})
	err := r.ChooseShare(context.Background(), store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}})
	if !errors.Is(err, ErrInvalidChoice) {
		t.Errorf("err = %v", err)
	}
	v, _ := r.ShareStep(context.Background())
	if v.Listed || !strings.Contains(v.ListError, "tmux is not installed") {
		t.Errorf("view: %+v", v)
	}
	// Share all still works when the sessions cannot be listed.
	if err := r.ChooseShare(context.Background(), store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Errorf("all without a listing: %v", err)
	}
}

func TestNoListerMeansCannotList(t *testing.T) {
	w := New(Config{Persist: &fakePersister{}})
	v, err := w.ShareStep(context.Background())
	if err != nil || v.Listed {
		t.Errorf("view %+v err %v", v, err)
	}
}

// The exact names are saved: hostile or odd names are not rewritten, and only
// the display is escaped.
func TestSaveKeepsTheExactSessionNames(t *testing.T) {
	ctx := context.Background()
	odd := "we\x1b[31mird\nname " + string(rune(0x202e))
	r := newRig([]string{"plain", odd}, Provided{})
	if err := r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{odd}}); err != nil {
		t.Fatal(err)
	}
	if err := r.w.SkipHub(); err != nil {
		t.Fatal(err)
	}
	if err := r.w.Save(ctx); err != nil {
		t.Fatal(err)
	}
	got := r.per.saved()
	if len(got) != 1 || got[0].Share.Mode != store.ShareSelect || len(got[0].Share.Sessions) != 1 || got[0].Share.Sessions[0] != odd {
		t.Fatalf("saved = %+v, want exactly the chosen name %q", got, odd)
	}
	if !got[0].Share.Configured {
		t.Error("the saved choice must be marked configured")
	}
	shown := DisplayName(odd)
	if strings.ContainsAny(shown, "\x1b\n"+string(rune(0x202e))) {
		t.Errorf("DisplayName leaves control characters: %q", shown)
	}
}

func TestDisplayNameShortensAndEscapes(t *testing.T) {
	if got := DisplayName("build"); got != "build" {
		t.Errorf("plain name changed: %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := DisplayName(long); len([]rune(got)) > 60 {
		t.Errorf("long name not shortened: %d", len([]rune(got)))
	}
	if got := DisplayName("a\xffb"); strings.ContainsRune(got, 0xff) || !strings.Contains(got, "\\u") {
		t.Errorf("invalid byte not escaped: %q", got)
	}
}

func TestHubCheckThenAcceptOrCorrect(t *testing.T) {
	ctx := context.Background()
	r := newRig([]string{"a"}, Provided{})
	r.chk.reports["https://bad.example:1"] = failedReport("")
	_ = r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll})

	if _, err := r.w.CheckHub(ctx, "ftp://hub.example"); !errors.Is(err, ErrInvalidChoice) {
		t.Errorf("a bad scheme: %v", err)
	}
	if _, err := r.w.CheckHub(ctx, "  "); !errors.Is(err, ErrInvalidChoice) {
		t.Errorf("an empty address is not a check: %v", err)
	}
	if _, err := r.w.CheckHub(ctx, "https://user:pw@hub.example"); !errors.Is(err, ErrInvalidChoice) {
		t.Errorf("credentials in the address: %v", err)
	}
	if got := r.chk.called(); len(got) != 0 {
		t.Errorf("an invalid address must not be checked: %v", got)
	}
	rep, err := r.w.CheckHub(ctx, "https://bad.example:1")
	if err != nil || !rep.Failed() {
		t.Fatalf("report %+v err %v", rep, err)
	}
	// Correct it: a new check replaces the candidate, and only the accepted address is saved.
	if _, err := r.w.CheckHub(ctx, "https://hub.example:8443"); err != nil {
		t.Fatal(err)
	}
	if err := r.w.UseHub(); err != nil {
		t.Fatal(err)
	}
	if err := r.w.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "https://hub.example:8443" {
		t.Errorf("saved = %+v", got)
	}
}

func TestSaveAnywayKeepsAFailedAddress(t *testing.T) {
	ctx := context.Background()
	r := newRig(nil, Provided{})
	r.chk.reports["https://bad.example:1"] = failedReport("")
	_ = r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareNone})
	if _, err := r.w.CheckHub(ctx, "https://bad.example:1"); err != nil {
		t.Fatal(err)
	}
	if err := r.w.UseHub(); err != nil {
		t.Fatal(err)
	}
	if err := r.w.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.per.saved(); len(got) != 1 || got[0].Server != "https://bad.example:1" {
		t.Errorf("saved = %+v", got)
	}
}

func TestSaveErrorIsReportedAndNothingIsClaimedSaved(t *testing.T) {
	ctx := context.Background()
	r := newRig(nil, Provided{})
	r.per.saveErr = errors.New("disk full")
	_ = r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll})
	_ = r.w.SkipHub()
	if err := r.w.Save(ctx); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.w.Pair(ctx, io.Discard); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("pairing must not start after a failed save: %v", err)
	}
}

func TestPairSoloDoesNothing(t *testing.T) {
	ctx := context.Background()
	r := newRig(nil, Provided{})
	_ = r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll})
	_ = r.w.SkipHub()
	_ = r.w.Save(ctx)
	out, err := r.w.Pair(ctx, io.Discard)
	if err != nil || out.Skipped != "solo" || out.Paired {
		t.Errorf("outcome %+v err %v", out, err)
	}
	if len(r.pair.calls()) != 0 {
		t.Error("a Solo Worker must not start pairing")
	}
}

func TestPairAlreadyPaired(t *testing.T) {
	ctx := context.Background()
	r := newRig(nil, Provided{})
	r.pair.already = &Paired{Owner: "bob"}
	_ = r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll})
	_, _ = r.w.CheckHub(ctx, "https://hub.example")
	_ = r.w.UseHub()
	_ = r.w.Save(ctx)
	out, err := r.w.Pair(ctx, io.Discard)
	if err != nil || !out.Paired || !out.Already || out.Owner != "bob" {
		t.Errorf("outcome %+v err %v", out, err)
	}
	if len(r.pair.calls()) != 0 {
		t.Error("an already paired Worker must not be offered a code")
	}
}

func TestPairUnavailableWithoutAPairingPort(t *testing.T) {
	ctx := context.Background()
	w := New(Config{Persist: &fakePersister{}, Check: &fakeChecker{}})
	_ = w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll})
	_, _ = w.CheckHub(ctx, "https://hub.example")
	_ = w.UseHub()
	_ = w.Save(ctx)
	out, err := w.Pair(ctx, io.Discard)
	if err != nil || out.Skipped != "unavailable" {
		t.Errorf("outcome %+v err %v", out, err)
	}
}

func TestPairWaitsAndStopsWithTheContext(t *testing.T) {
	r := newRig(nil, Provided{})
	r.pair.wait = true
	ctx := context.Background()
	_ = r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll})
	_, _ = r.w.CheckHub(ctx, "https://hub.example")
	_ = r.w.UseHub()
	_ = r.w.Save(ctx)

	pctx, cancel := context.WithCancel(ctx)
	type result struct {
		out PairOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		o, e := r.w.Pair(pctx, io.Discard)
		done <- result{o, e}
	}()
	<-r.pair.started // the code is showing and the pairing waits
	cancel()
	select {
	case res := <-done:
		if res.err != nil || res.out.Paired {
			t.Errorf("a stopped pairing is not an error and not paired: %+v %v", res.out, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Pair did not return after its context ended")
	}
}

func TestPairErrorIsReported(t *testing.T) {
	r := newRig(nil, Provided{})
	r.pair.err = errors.New("the Hub removed this Worker key")
	ctx := context.Background()
	_ = r.w.ChooseShare(ctx, store.ShareSelection{Mode: store.ShareAll})
	_, _ = r.w.CheckHub(ctx, "https://hub.example")
	_ = r.w.UseHub()
	_ = r.w.Save(ctx)
	if _, err := r.w.Pair(ctx, io.Discard); err == nil || !strings.Contains(err.Error(), "removed this Worker key") {
		t.Errorf("err = %v", err)
	}
}

func TestProvidedShareIsAppliedWithoutAQuestion(t *testing.T) {
	ctx := context.Background()
	r := newRig([]string{"a"}, Provided{Share: store.ShareLater})
	v, _ := r.w.ShareStep(ctx)
	if v.Decided != store.ShareLater {
		t.Fatalf("decided = %q", v.Decided)
	}
	if err := r.w.ApplyProvided(ctx); err != nil {
		t.Fatal(err)
	}
	hv, err := r.w.HubStep(ctx)
	if err != nil || hv.Decided {
		t.Errorf("hub view %+v err %v", hv, err)
	}
	r2 := newRig(nil, Provided{Share: "sometimes"})
	if err := r2.w.ApplyProvided(ctx); !errors.Is(err, ErrInvalidChoice) {
		t.Errorf("an invalid provided share: %v", err)
	}
}

func TestDescribeShareIsPlainEnglish(t *testing.T) {
	for sel, want := range map[string]string{
		string(store.ShareAll):   "all tmux sessions",
		string(store.ShareNone):  "no tmux session",
		string(store.ShareLater): "nothing is shared until you choose",
	} {
		if got := DescribeShare(store.ShareSelection{Mode: store.ShareMode(sel)}); !strings.Contains(got, want) {
			t.Errorf("%s: %q lacks %q", sel, got, want)
		}
	}
	got := DescribeShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"b", "a"}})
	if !strings.Contains(got, "2 sessions") || !strings.Contains(got, "a, b") {
		t.Errorf("select: %q", got)
	}
}
