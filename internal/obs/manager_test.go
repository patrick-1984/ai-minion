package obs

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

// A change publishes the new pipeline with one slot update: the slot is never
// empty while the sink stays on, so no record is lost unseen.
func TestApplyNeverEmptiesAnEnabledSlot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	_, live, _ := logx.NewLiveLocal(logx.Options{Output: io.Discard})
	m := New(live.Fanout, Process{Service: "minion-hub"})
	defer func() { _ = m.Shutdown(context.Background()) }()
	cfg := func(level string) config.Sinks {
		return config.Sinks{Syslog: config.SyslogConfig{Enabled: true, Network: "tcp", Addr: ln.Addr().String(), Level: level,
			MaxBytes: 2048, Digest: "d-" + level}}
	}
	if err := m.Apply(context.Background(), cfg("info")); err != nil {
		t.Fatal(err)
	}
	var empty atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if live.Fanout.Handler(logx.SlotSyslog) == nil {
					empty.Add(1)
				}
			}
		}
	}()
	for i := range 50 {
		level := "info"
		if i%2 == 0 {
			level = "warn"
		}
		if err := m.Apply(context.Background(), cfg(level)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if empty.Load() != 0 {
		t.Fatalf("the slot was empty %d times during level changes", empty.Load())
	}
	if st := m.Status()[0]; st.Generation != 51 || !st.Enabled {
		t.Fatalf("status = %+v", st)
	}
}

// Disable retires both sinks and shows the reason.
func TestDisableFailsClosed(t *testing.T) {
	_, live, _ := logx.NewLiveLocal(logx.Options{Output: io.Discard})
	m := New(live.Fanout, Process{Service: "minion-hub"})
	_ = m.Apply(context.Background(), config.Sinks{Syslog: config.SyslogConfig{Enabled: true, Network: "udp", Addr: "127.0.0.1:9", Level: "info", MaxBytes: 2048, Digest: "x"}})
	m.Disable("setting \"otel_ca_file\": cannot read")
	st := m.Status()
	if live.Fanout.Handler(logx.SlotSyslog) != nil || st[0].Enabled || st[0].LastError == "" || st[1].LastError == "" {
		t.Fatalf("status = %+v", st)
	}
}
