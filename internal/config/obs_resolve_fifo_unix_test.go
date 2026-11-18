//go:build unix

package config

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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

