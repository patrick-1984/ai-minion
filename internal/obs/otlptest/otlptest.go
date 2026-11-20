// Package otlptest is an in-process OTLP/HTTP logs receiver for tests.
package otlptest

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

// Event is one received log record.
type Event struct {
	Name, Severity, Body string
	Attrs                map[string]string
}

// Sink is the receiver. While stalled it does not answer.
type Sink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []Event
	hold chan struct{}
}

// New starts a receiver that is closed with the test.
func New(t *testing.T) *Sink {
	s := &Sink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		hold := s.hold
		s.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			body = zr
		}
		data, _ := io.ReadAll(body)
		var m collogs.ExportLogsServiceRequest
		if proto.Unmarshal(data, &m) != nil {
			w.WriteHeader(400)
			return
		}
		s.mu.Lock()
		for _, rl := range m.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					e := Event{Name: lr.EventName, Severity: lr.SeverityText, Body: lr.Body.GetStringValue(), Attrs: map[string]string{}}
					for _, kv := range lr.Attributes {
						e.Attrs[kv.Key] = kv.Value.GetStringValue()
						if e.Attrs[kv.Key] == "" {
							e.Attrs[kv.Key] = strings.TrimSpace(kv.Value.String())
						}
					}
					s.got = append(s.got, e)
				}
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&collogs.ExportLogsServiceResponse{})
		_, _ = w.Write(out)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// Events returns what was received so far.
func (s *Sink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.got...)
}

// URL is the logs endpoint.
func (s *Sink) URL() string { return s.srv.URL + "/v1/logs" }

// Stall makes the receiver stop answering until Release.
func (s *Sink) Stall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = make(chan struct{})
}

// Release ends a stall.
func (s *Sink) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold != nil {
		close(s.hold)
		s.hold = nil
	}
}

// WaitEvents waits until n events arrived, lets a further batch interval pass
// and fails the test unless exactly n arrived.
func (s *Sink) WaitEvents(t *testing.T, n int) []Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(s.Events()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("got %d events, want %d: %+v", len(s.Events()), n, s.Events())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	got := s.Events()
	if len(got) != n {
		t.Fatalf("got %d events, want %d: %+v", len(got), n, got)
	}
	return got
}
