//go:build unix

package config

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFIFOIsRefusedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ca.pem")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	_, err := ResolveSinks(hubRes(nil, map[string]string{"syslog": "on", "syslog_endpoint": "tls://h:6514", "syslog_ca_file": fifo}), SinkFiles{})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	_, err = ResolveSinks(hubRes(map[string]string{"MINION_OTEL_HEADERS_FILE": fifo}, map[string]string{"otel": "on", "otel_endpoint": "http://127.0.0.1:1"}), SinkFiles{})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("headers fifo: err = %v", err)
	}
}

// The generic secret reader (settings_list resolves otel_headers through it
// with OTLP off) never blocks on a FIFO.
func TestSecretFileFIFODoesNotBlock(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "h")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	res := hubRes(map[string]string{"MINION_OTEL_HEADERS_FILE": fifo}, nil)
	done := make(chan error, 1)
	go func() { _, err := res.Resolve(KeyOTelHeaders); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the secret reader blocked on a FIFO")
	}
}
