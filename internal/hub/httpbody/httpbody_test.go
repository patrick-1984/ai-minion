package httpbody

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestArmOnlyTouchesRequestsWithABody(t *testing.T) {
	rec := httptest.NewRecorder()
	if Arm(rec, httptest.NewRequest(http.MethodGet, "/", nil), time.Second) {
		t.Fatal("a request without a body was armed")
	}
	r := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	if Arm(rec, r, time.Second) {
		t.Fatal("a request with http.NoBody was armed")
	}
	// A writer that cannot set a deadline (a recorder) has no connection to
	// stall: the body is left as it was.
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
	orig := r.Body
	if Arm(rec, r, time.Second) || r.Body != orig {
		t.Fatal("a recorder was armed or its body replaced")
	}
}

// deadlineWriter records the read deadlines that are set on it.
type deadlineWriter struct {
	http.ResponseWriter
	set []time.Time
}

func (d *deadlineWriter) SetReadDeadline(t time.Time) error {
	d.set = append(d.set, t)
	return nil
}

func TestArmSetsTheDeadlineBeforeTheReadAndLiftsItAtEOF(t *testing.T) {
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	before := time.Now()
	if !Arm(w, r, 5*time.Second) {
		t.Fatal("not armed")
	}
	if len(w.set) != 1 || w.set[0].Before(before.Add(4*time.Second)) || w.set[0].After(time.Now().Add(6*time.Second)) {
		t.Fatalf("deadlines = %v, want one about 5 s ahead", w.set)
	}
	// Armed already (by the guard): left as it is.
	if !Arm(w, r, time.Hour) || len(w.set) != 1 {
		t.Fatalf("a second Arm set another deadline: %v", w.set)
	}
	buf := make([]byte, 3)
	if _, err := r.Body.Read(buf); err != nil || len(w.set) != 1 {
		t.Fatalf("a partial read lifted the deadline: %v, %v", err, w.set)
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		t.Fatal(err)
	}
	if len(w.set) != 2 || !w.set[1].IsZero() {
		t.Fatalf("deadlines = %v, want the second one lifted (zero)", w.set)
	}
}

// A value parsed from the start of a body says nothing about the rest of it:
// the deadline stays in force until the body is read to its end, and only that
// lifts it. There is no public way to lift it by hand.
func TestTheDeadlineStaysUntilTheBodyReallyEnds(t *testing.T) {
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`+"\n"))
	Arm(w, r, time.Second)
	_, _ = r.Body.Read(make([]byte, 7)) // a decoder stops after the object
	if len(w.set) != 1 {
		t.Fatalf("deadlines = %v: a decoded value lifted the deadline", w.set)
	}
	if err := Drain(r); err != nil {
		t.Fatal(err)
	}
	if len(w.set) != 2 || !w.set[1].IsZero() {
		t.Fatalf("deadlines = %v, want the end of the body to lift it", w.set)
	}
}

// Drain returns the error of a body that does not end (a read that fails) and
// leaves the deadline in force.
func TestDrainOfABodyThatFailsKeepsTheDeadline(t *testing.T) {
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(io.MultiReader(strings.NewReader("ab"), failing{})))
	Arm(w, r, time.Second)
	if err := Drain(r); err == nil {
		t.Fatal("Drain of a failing body succeeded")
	}
	if len(w.set) != 1 {
		t.Fatalf("deadlines = %v: a failed body lifted the deadline", w.set)
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestGuardArmsBeforeTheHandlerRuns(t *testing.T) {
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	var armedInHandler bool
	h := Guard(2 * time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		armedInHandler = len(w.(*deadlineWriter).set) == 1
	}))
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader("{")))
	if !armedInHandler {
		t.Fatal("the deadline was not armed when the handler started")
	}
	w2 := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/worker/connect", nil))
	if len(w2.set) != 0 {
		t.Fatalf("a request without a body got a deadline: %v", w2.set)
	}
}
