//go:build linux && !android

package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

func startMaster(parent context.Context, env *Env) (StopFunc, error) {
	host, _ := Require[term.SessionHost](env)
	h, ok := host.(*tmux.Host)
	if !ok || !masterPeerSupported() {
		return nil, nil
	}
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}
	paths, err := localapi.MasterSocketPaths(config.SystemPathEnv())
	if err != nil {
		return nil, err
	}
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return nil, err
	}
	ln, err := localapi.Listen(paths[0])
	if err != nil {
		// This optional endpoint cannot prevent an ordinary Worker from serving.
		env.Settings().logger().Warn("Minion master socket could not be started", "error", err)
		return nil, nil
	}
	b := newMasterBroker(h, ex, self)
	if err := Provide(env, &masterIntegration{serve: b.serveLink, files: b.fileCall, apps: b.appCall}); err != nil {
		_ = ln.Close()
		return nil, err
	}
	ex.masterShareChanged = b.revokeUnshared
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.run(ctx, ln)
	}()
	return func(stop context.Context) error {
		cancel()
		_ = ln.Close()
		select {
		case <-done:
			return nil
		case <-stop.Done():
			return stop.Err()
		}
	}, nil
}

func (b *masterBroker) run(ctx context.Context, ln net.Listener) {
	ctx, cancel := context.WithCancel(ctx)
	var jobs sync.WaitGroup
	defer func() {
		cancel()
		jobs.Wait()
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, a := range b.attachments {
			b.retire(a)
		}
	}()
	jobs.Go(func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				b.reconcile(ctx)
			}
		}
	})
	slots := make(chan struct{}, 32)
	for {
		conn, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
			return
		}
		if err != nil {
			continue
		}
		select {
		case slots <- struct{}{}:
			jobs.Go(func() {
				defer func() { <-slots }()
				defer func() { _ = conn.Close() }()
				unixConn, ok := conn.(*net.UnixConn)
				if ok {
					b.serveLocal(ctx, unixConn)
				}
			})
		default:
			_ = conn.Close()
		}
	}
}
