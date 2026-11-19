//go:build unix

package otel

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An inherited OTEL_EXPORTER_OTLP_*CERTIFICATE path on a stalled mount (here
// a FIFO nobody writes to) does not hang Configure, and Shutdown does not wait
// for a Configure that is stuck.
func TestMetricsBuildWithAStalledCertificatePathIsBounded(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "ca")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", fifo)
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", fifo)
	old := buildTimeout
	buildTimeout = 300 * time.Millisecond
	defer func() { buildTimeout = old }()
	rcv := newMetricsReceiver(t)
	m := NewMetrics()
	done := make(chan error, 1)
	go func() { done <- m.Configure(context.Background(), metricsConfig(t, rcv.srv.URL, MetricsBasic), nil) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Configure hung on the certificate path")
	}
	t.Logf("configure: %v", err)
	sd := make(chan struct{})
	go func() { _ = m.Shutdown(context.Background()); close(sd) }()
	select {
	case <-sd:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown waited")
	}
	// release anything still blocked on the FIFO
	if f, e := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); e == nil {
		_ = f.Close()
	}
}
